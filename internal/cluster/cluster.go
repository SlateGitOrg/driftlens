// Package cluster reads live objects from a Kubernetes API server and
// offers server-side (dry-run) defaulting.
package cluster

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/SlateGitOrg/driftlens/pkg/manifest"
)

// Client reads live objects. It implements drift.LiveSource.
type Client struct {
	dyn    dynamic.Interface
	mapper meta.ResettableRESTMapper
}

// Load builds a REST config from a kubeconfig file (empty: the default
// loading rules, honouring $KUBECONFIG) and an optional context name.
func Load(kubeconfig, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

// New returns a client for cfg.
func New(cfg *rest.Config) (*Client, error) {
	cfg = rest.CopyConfig(cfg)
	if cfg.UserAgent == "" {
		cfg.UserAgent = "driftlens"
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}, nil
}

func (c *Client) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		// A CRD may have been installed after discovery was cached.
		c.mapper.Reset()
		m, err = c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", gvk, err)
	}
	return m, nil
}

func (c *Client) resource(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	m, err := c.mapping(gvk)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return c.dyn.Resource(m.Resource).Namespace(namespace), nil
	}
	return c.dyn.Resource(m.Resource), nil
}

// Namespaced reports whether gvk is a namespaced kind.
func (c *Client) Namespaced(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	m, err := c.mapping(gvk)
	if err != nil {
		return false, err
	}
	return m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// Get fetches the live object named by ref in ref's API version. A missing
// object yields (nil, nil).
func (c *Client) Get(ctx context.Context, ref manifest.Ref) (*unstructured.Unstructured, error) {
	ri, err := c.resource(ref.GVK(), ref.Namespace)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}
	return u, nil
}

// Snapshot fetches the live counterparts of desired objects, skipping the
// ones that do not exist. Namespace-less namespaced objects use defaultNS.
func (c *Client) Snapshot(ctx context.Context, desired []*unstructured.Unstructured, defaultNS string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, d := range desired {
		ref := manifest.RefOf(d)
		if ref.Namespace == "" {
			ns, err := c.Namespaced(ctx, ref.GVK())
			if err != nil {
				return nil, err
			}
			if ns {
				ref.Namespace = defaultNS
			}
		}
		u, err := c.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		if u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// ServerDefaulter defaults objects by asking the API server: it performs a
// dry-run create of a renamed copy (generateName) and returns the result
// with the original name restored. This picks up every built-in default,
// CRD structural-schema defaults and mutating admission, at the cost of one
// request per object and `create` permission.
type ServerDefaulter struct {
	Client *Client
}

// Name implements normalize.Defaulter.
func (ServerDefaulter) Name() string { return "server" }

// Default implements normalize.Defaulter.
func (s ServerDefaulter) Default(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := s.Client.resource(obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	probe := obj.DeepCopy()
	name := probe.GetName()
	probe.SetName("")
	probe.SetGenerateName(generatePrefix(name))
	probe.SetResourceVersion("")
	out, err := ri.Create(ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: "driftlens-dryrun"})
	if err != nil {
		return nil, fmt.Errorf("server dry-run of %s: %w", manifest.RefOf(obj), err)
	}
	out.SetName(name)
	out.SetGenerateName(obj.GetGenerateName())
	if obj.GetGenerateName() == "" {
		delete(out.Object["metadata"].(map[string]any), "generateName")
	}
	return out, nil
}

// generatePrefix keeps generated names valid for every kind (DNS-1035
// labels are the strictest: at most 63 characters, starting with a letter).
func generatePrefix(name string) string {
	p := strings.ToLower(name)
	if len(p) > 40 {
		p = p[:40]
	}
	p = strings.TrimRight(p, "-.")
	if p == "" || p[0] < 'a' || p[0] > 'z' {
		p = "dl" + p
	}
	return p + "-dl-"
}

// Apply server-side-applies obj as fieldManager (with force, like a GitOps
// controller would).
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured, fieldManager string) (*unstructured.Unstructured, error) {
	ri, err := c.resource(obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	force := true
	out, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: force})
	if err != nil {
		return nil, fmt.Errorf("apply %s: %w", manifest.RefOf(obj), err)
	}
	return out, nil
}

// CreateWithLastApplied creates obj the way `kubectl apply` (client-side)
// does on first apply: the object carries the last-applied-configuration
// annotation and the request is an Update-operation create.
func (c *Client) CreateWithLastApplied(ctx context.Context, obj *unstructured.Unstructured, fieldManager string) (*unstructured.Unstructured, error) {
	ri, err := c.resource(obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	o := obj.DeepCopy()
	raw, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	ann := o.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["kubectl.kubernetes.io/last-applied-configuration"] = string(raw)
	o.SetAnnotations(ann)
	out, err := ri.Create(ctx, o, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", manifest.RefOf(obj), err)
	}
	return out, nil
}

// Patch patches the object named by ref as fieldManager. subresource may be
// empty or e.g. "scale".
func (c *Client) Patch(ctx context.Context, ref manifest.Ref, pt types.PatchType, data []byte, fieldManager, subresource string) error {
	ri, err := c.resource(ref.GVK(), ref.Namespace)
	if err != nil {
		return err
	}
	var sub []string
	if subresource != "" {
		sub = []string{subresource}
	}
	if _, err := ri.Patch(ctx, ref.Name, pt, data, metav1.PatchOptions{FieldManager: fieldManager}, sub...); err != nil {
		return fmt.Errorf("patch %s: %w", ref, err)
	}
	return nil
}

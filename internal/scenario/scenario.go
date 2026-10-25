// Package scenario drives reproducible drift scenarios against a real API
// server: apply a Git revision the way a GitOps tool would, then mutate the
// live objects the way people and controllers do. The golden tests, the
// envtest suite and the demo all use it.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/SlateGitOrg/driftlens/internal/cluster"
	"github.com/SlateGitOrg/driftlens/pkg/manifest"
	"github.com/SlateGitOrg/driftlens/pkg/rules"
)

// Target names the object a mutation acts on.
type Target struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// Ref converts the target to a manifest reference.
func (t Target) Ref() (manifest.Ref, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(t.APIVersion)
	u.SetKind(t.Kind)
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" || gvk.Version == "" || t.Name == "" {
		return manifest.Ref{}, fmt.Errorf("mutation target needs apiVersion, kind and name: %+v", t)
	}
	return manifest.Ref{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: t.Namespace, Name: t.Name}, nil
}

// Mutation is one out-of-band write to a live object.
type Mutation struct {
	Manager     string           `json:"manager"`
	Target      Target           `json:"target"`
	Subresource string           `json:"subresource,omitempty"`
	MergePatch  map[string]any   `json:"mergePatch,omitempty"`
	JSONPatch   []map[string]any `json:"jsonPatch,omitempty"`
}

// Spec is the scenario.yaml file.
type Spec struct {
	Description string `json:"description"`
	// Apply is "ssa" (server-side apply, default) or "csa" (client-side
	// apply with the last-applied annotation).
	Apply string `json:"apply,omitempty"`
	// Manager is the GitOps field manager (default: driftlens for ssa,
	// kubectl-client-side-apply for csa).
	Manager   string     `json:"manager,omitempty"`
	Mutations []Mutation `json:"mutations,omitempty"`
	// DesiredKustomize builds the desired state from this kustomization
	// directory (relative to the scenario) instead of desired.yaml.
	DesiredKustomize string `json:"desiredKustomize,omitempty"`
	// Defaults selects the defaulter used by tests: static (default) or none.
	Defaults string `json:"defaults,omitempty"`
}

// Scenario is a loaded scenario directory.
type Scenario struct {
	Name    string
	Dir     string
	Spec    Spec
	CRDs    []*unstructured.Unstructured // crds.yaml, installed first
	Applied []*unstructured.Unstructured // applied.yaml: the last Git revision applied
	Desired []*unstructured.Unstructured // desired.yaml (or the kustomization): Git now
	Config  *rules.Config                // config.yaml, or the default
}

// Load reads a scenario directory.
func Load(dir string) (*Scenario, error) {
	s := &Scenario{Name: filepath.Base(dir), Dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "scenario.yaml")) // #nosec G304 -- test fixture path
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, &s.Spec); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if s.Spec.Apply == "" {
		s.Spec.Apply = "ssa"
	}
	if s.Spec.Apply != "ssa" && s.Spec.Apply != "csa" {
		return nil, fmt.Errorf("%s: apply must be ssa or csa", dir)
	}
	if s.Spec.Manager == "" {
		s.Spec.Manager = map[string]string{"ssa": "driftlens", "csa": "kubectl-client-side-apply"}[s.Spec.Apply]
	}
	if s.CRDs, err = optional(filepath.Join(dir, "crds.yaml")); err != nil {
		return nil, err
	}
	if s.Applied, err = manifest.Load([]string{filepath.Join(dir, "applied.yaml")}, nil); err != nil {
		return nil, err
	}
	switch {
	case s.Spec.DesiredKustomize != "":
		s.Desired, err = manifest.BuildKustomization(filepath.Join(dir, s.Spec.DesiredKustomize))
	default:
		s.Desired, err = optional(filepath.Join(dir, "desired.yaml"))
		if err == nil && s.Desired == nil {
			s.Desired = s.Applied
		}
	}
	if err != nil {
		return nil, err
	}
	s.Config = rules.Default()
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
		if s.Config, err = rules.Load(filepath.Join(dir, "config.yaml")); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func optional(file string) ([]*unstructured.Unstructured, error) {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return manifest.Load([]string{file}, nil)
}

// Setup installs CRDs, creates namespaces, applies the applied revision and
// performs the mutations.
func (s *Scenario) Setup(ctx context.Context, c *cluster.Client) error {
	for _, crd := range s.CRDs {
		if _, err := c.Apply(ctx, crd, "scenario-setup"); err != nil {
			return err
		}
	}
	for _, crd := range s.CRDs {
		if err := waitEstablished(ctx, c, crd); err != nil {
			return err
		}
	}
	nsSeen := map[string]bool{}
	for _, o := range append(append([]*unstructured.Unstructured{}, s.Applied...), s.Desired...) {
		if ns := o.GetNamespace(); ns != "" && !nsSeen[ns] {
			nsSeen[ns] = true
			n := &unstructured.Unstructured{}
			n.SetAPIVersion("v1")
			n.SetKind("Namespace")
			n.SetName(ns)
			if _, err := c.Apply(ctx, n, "scenario-setup"); err != nil {
				return err
			}
		}
	}
	for _, o := range s.Applied {
		var err error
		if s.Spec.Apply == "csa" {
			_, err = c.CreateWithLastApplied(ctx, o, s.Spec.Manager)
		} else {
			_, err = c.Apply(ctx, o, s.Spec.Manager)
		}
		if err != nil {
			return err
		}
	}
	for i, m := range s.Spec.Mutations {
		if err := mutate(ctx, c, m); err != nil {
			return fmt.Errorf("mutation %d (%s): %w", i, m.Manager, err)
		}
	}
	return nil
}

func mutate(ctx context.Context, c *cluster.Client, m Mutation) error {
	ref, err := m.Target.Ref()
	if err != nil {
		return err
	}
	switch {
	case m.MergePatch != nil && m.JSONPatch == nil:
		b, err := json.Marshal(m.MergePatch)
		if err != nil {
			return err
		}
		return c.Patch(ctx, ref, types.MergePatchType, b, m.Manager, m.Subresource)
	case m.JSONPatch != nil && m.MergePatch == nil:
		b, err := json.Marshal(m.JSONPatch)
		if err != nil {
			return err
		}
		return c.Patch(ctx, ref, types.JSONPatchType, b, m.Manager, m.Subresource)
	}
	return errors.New("exactly one of mergePatch or jsonPatch is required")
}

func waitEstablished(ctx context.Context, c *cluster.Client, crd *unstructured.Unstructured) error {
	ref := manifest.RefOf(crd)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		u, err := c.Get(ctx, ref)
		if err != nil {
			return err
		}
		if u != nil {
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			for _, cnd := range conds {
				m, _ := cnd.(map[string]any)
				if m["type"] == "Established" && m["status"] == "True" {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("CRD %s not established after 30s", ref.Name)
}

// Scrub replaces per-run values (UIDs, resource versions, timestamps) with
// fixed ones so that captured snapshots are reproducible files.
func Scrub(objs []*unstructured.Unstructured) {
	for _, o := range objs {
		o.SetUID("00000000-0000-0000-0000-000000000000")
		o.SetResourceVersion("1")
		md, _ := o.Object["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		md["creationTimestamp"] = "2026-01-01T00:00:00Z"
		if mf, ok := md["managedFields"].([]any); ok {
			for _, e := range mf {
				if em, ok := e.(map[string]any); ok {
					em["time"] = "2026-01-01T00:00:00Z"
				}
			}
		}
	}
}

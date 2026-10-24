// Package normalize turns desired, live and last-applied objects into a
// comparable form: it strips server-managed metadata and status, removes
// server-assigned fields the desired state does not set, canonicalizes
// numbers, quantities and empty values, and fills in API defaults.
package normalize

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utiljson "k8s.io/apimachinery/pkg/util/json"

	"github.com/archit2501/driftlens/pkg/objpath"
)

// LastAppliedAnnotation is written by `kubectl apply` (client-side apply).
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// serverMetadata lists metadata fields the API server owns.
var serverMetadata = []string{
	"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp",
	"deletionGracePeriodSeconds", "selfLink", "managedFields",
}

// serverAnnotations are bookkeeping annotations written by tools or controllers.
var serverAnnotations = []string{
	LastAppliedAnnotation,
	"deployment.kubernetes.io/revision",
}

// assigned lists fields the server (or a built-in controller/admission
// plugin) fills in. They are removed from live and last-applied objects
// unless the desired object sets them itself.
var assigned = map[string][]*objpath.Pattern{
	"/Service": compileAll(
		".spec.clusterIP", ".spec.clusterIPs", ".spec.ipFamilies", ".spec.ipFamilyPolicy",
		".spec.ports[].nodePort", ".spec.healthCheckNodePort",
	),
	"/PersistentVolumeClaim": compileAll(
		".spec.volumeName", ".spec.storageClassName",
		`.metadata.annotations["pv.kubernetes.io/bind-completed"]`,
		`.metadata.annotations["pv.kubernetes.io/bound-by-controller"]`,
		`.metadata.annotations["volume.beta.kubernetes.io/storage-provisioner"]`,
		`.metadata.annotations["volume.kubernetes.io/storage-provisioner"]`,
		".metadata.finalizers",
	),
	"batch/Job": compileAll(
		".spec.selector",
		`.spec.template.metadata.labels["controller-uid"]`,
		`.spec.template.metadata.labels["batch.kubernetes.io/controller-uid"]`,
		`.spec.template.metadata.labels["batch.kubernetes.io/job-name"]`,
		`.spec.template.metadata.labels["job-name"]`,
	),
	"/ServiceAccount": compileAll(".secrets"),
	"/Namespace": compileAll(
		".spec.finalizers",
		`.metadata.labels["kubernetes.io/metadata.name"]`,
	),
	"/Pod":             compileAll(".spec.nodeName", ".spec.serviceAccount"),
	"apps/StatefulSet": compileAll(".spec.volumeClaimTemplates[].status"),
	"apps/DaemonSet":   compileAll(`.metadata.annotations["deprecated.daemonset.template.generation"]`),
}

func compileAll(srcs ...string) []*objpath.Pattern {
	out := make([]*objpath.Pattern, len(srcs))
	for i, s := range srcs {
		out[i] = objpath.MustCompile(s)
	}
	return out
}

// emptyMapKeys are map-typed fields (Go maps in the API types) that the API
// server omits when empty, so "{}" and "absent" mean the same thing.
var emptyMapKeys = map[string]bool{
	"labels": true, "annotations": true, "data": true, "binaryData": true, "stringData": true,
	"nodeSelector": true, "matchLabels": true, "requests": true, "limits": true, "selector": true,
	"metadata": true, // nested template metadata ({} after dropping creationTimestamp: null)
}

// quantityParents are map fields whose values are resource.Quantity strings.
var quantityParents = map[string]bool{
	"requests": true, "limits": true, "hard": true, "capacity": true, "allocatable": true,
}

// StripServerFields removes server-managed metadata, bookkeeping annotations
// and status from obj, in place.
func StripServerFields(obj map[string]any) {
	delete(obj, "status")
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, f := range serverMetadata {
		delete(md, f)
	}
	if ann, ok := md["annotations"].(map[string]any); ok {
		for _, a := range serverAnnotations {
			delete(ann, a)
		}
	}
}

// StripAssigned removes server-assigned fields from obj (a live or
// last-applied object) unless desired sets them.
func StripAssigned(group, kind string, obj, desired map[string]any) {
	for _, p := range assigned[group+"/"+kind] {
		if desired != nil && len(p.Find(desired)) > 0 {
			continue
		}
		p.Prune(obj)
	}
}

// Canonicalize rewrites obj in place so that semantically equal objects are
// structurally equal: integral floats become int64, nulls and empty lists
// are dropped, empty Go-map fields are dropped, and resource quantities are
// rendered in canonical form ("1000m" -> "1", "1024Mi" -> "1Gi").
func Canonicalize(obj map[string]any) {
	canonMap(obj, "")
}

func canonMap(m map[string]any, parentKey string) {
	for k, v := range m {
		nv, keep := canonValue(v, k, parentKey)
		if !keep {
			delete(m, k)
			continue
		}
		m[k] = nv
	}
}

func canonValue(v any, key, parentKey string) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		canonMap(t, key)
		if len(t) == 0 && emptyMapKeys[key] {
			return nil, false
		}
		return t, true
	case []any:
		out := t[:0]
		for _, e := range t {
			ne, keep := canonValue(e, "", key)
			if keep {
				out = append(out, ne)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t), true
		}
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case string:
		if quantityParents[parentKey] || key == "sizeLimit" {
			if q, err := resource.ParseQuantity(t); err == nil {
				return q.String(), true
			}
		}
		return t, true
	}
	return v, true
}

// Prepared is a normalized object plus what normalization learned from it.
type Prepared struct {
	Object      *unstructured.Unstructured
	LastApplied *unstructured.Unstructured // parsed last-applied annotation, if any
	Managed     []any                      // raw metadata.managedFields, if any
}

// PrepareLive copies a live object, extracts its managedFields and
// last-applied annotation, and strips server-managed fields. desired is used
// to decide which server-assigned fields to keep (may be nil).
func PrepareLive(live *unstructured.Unstructured, desired map[string]any) (*Prepared, error) {
	obj := runtime.DeepCopyJSON(live.Object)
	p := &Prepared{}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if mf, ok := md["managedFields"].([]any); ok {
			p.Managed = mf
		}
		if ann, ok := md["annotations"].(map[string]any); ok {
			if raw, ok := ann[LastAppliedAnnotation].(string); ok && strings.TrimSpace(raw) != "" {
				la, err := decodeLastApplied(raw)
				if err != nil {
					return nil, err
				}
				p.LastApplied = la
			}
		}
	}
	StripServerFields(obj)
	gvk := live.GroupVersionKind()
	StripAssigned(gvk.Group, gvk.Kind, obj, desired)
	Canonicalize(obj)
	p.Object = &unstructured.Unstructured{Object: obj}
	return p, nil
}

// PrepareDesired copies a desired (or last-applied) object, strips any
// server-managed fields it carries and canonicalizes it.
func PrepareDesired(u *unstructured.Unstructured) *unstructured.Unstructured {
	obj := runtime.DeepCopyJSON(u.Object)
	StripServerFields(obj)
	if gvk := u.GroupVersionKind(); gvk.Group == "" && gvk.Kind == "Secret" {
		foldStringData(obj)
	}
	Canonicalize(obj)
	return &unstructured.Unstructured{Object: obj}
}

func decodeLastApplied(raw string) (*unstructured.Unstructured, error) {
	var obj map[string]any
	if err := utiljson.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("parse %s annotation: %w", LastAppliedAnnotation, err)
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// foldStringData mirrors what the API server does with Secret.stringData:
// each entry is base64-encoded into data (overriding data) and stringData is
// dropped, so a desired Secret written with stringData compares equal to the
// stored object.
func foldStringData(obj map[string]any) {
	sd, ok := obj["stringData"].(map[string]any)
	if !ok {
		return
	}
	data, _ := obj["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	for k, v := range sd {
		if s, ok := v.(string); ok {
			data[k] = base64.StdEncoding.EncodeToString([]byte(s))
		}
	}
	obj["data"] = data
	delete(obj, "stringData")
}

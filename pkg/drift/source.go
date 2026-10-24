package drift

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/archit2501/driftlens/pkg/manifest"
)

// clusterScoped lists built-in cluster-scoped kinds, used when no API server
// is available to answer RESTMapper questions.
var clusterScoped = map[string]bool{
	"/Namespace": true, "/Node": true, "/PersistentVolume": true, "/ComponentStatus": true,
	"rbac.authorization.k8s.io/ClusterRole":                     true,
	"rbac.authorization.k8s.io/ClusterRoleBinding":              true,
	"apiextensions.k8s.io/CustomResourceDefinition":             true,
	"apiregistration.k8s.io/APIService":                         true,
	"storage.k8s.io/StorageClass":                               true,
	"storage.k8s.io/CSIDriver":                                  true,
	"storage.k8s.io/CSINode":                                    true,
	"storage.k8s.io/VolumeAttachment":                           true,
	"scheduling.k8s.io/PriorityClass":                           true,
	"node.k8s.io/RuntimeClass":                                  true,
	"networking.k8s.io/IngressClass":                            true,
	"admissionregistration.k8s.io/ValidatingWebhookConfiguration":   true,
	"admissionregistration.k8s.io/MutatingWebhookConfiguration":     true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicy":        true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding": true,
	"certificates.k8s.io/CertificateSigningRequest":                 true,
	"flowcontrol.apiserver.k8s.io/FlowSchema":                       true,
	"flowcontrol.apiserver.k8s.io/PriorityLevelConfiguration":       true,
}

// SnapshotSource serves live objects from a saved snapshot (for example the
// output of `driftlens snapshot` or `kubectl get -o yaml`).
type SnapshotSource struct {
	objs   map[string]*unstructured.Unstructured
	scoped map[string]bool // group/kind -> namespaced, learned from the snapshot
}

// NewSnapshotSource indexes objects by group, kind, namespace and name.
func NewSnapshotSource(objs []*unstructured.Unstructured) *SnapshotSource {
	s := &SnapshotSource{objs: map[string]*unstructured.Unstructured{}, scoped: map[string]bool{}}
	for _, o := range objs {
		r := manifest.RefOf(o)
		s.objs[r.Key()] = o
		gk := r.Group + "/" + r.Kind
		if r.Namespace != "" {
			s.scoped[gk] = true
		} else if _, seen := s.scoped[gk]; !seen {
			s.scoped[gk] = false
		}
	}
	return s
}

// Get implements LiveSource.
func (s *SnapshotSource) Get(_ context.Context, ref manifest.Ref) (*unstructured.Unstructured, error) {
	o, ok := s.objs[ref.Key()]
	if !ok {
		return nil, nil
	}
	return o, nil
}

// Namespaced implements LiveSource: the snapshot's own objects decide, then
// the built-in table; unknown kinds are assumed namespaced.
func (s *SnapshotSource) Namespaced(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	gk := gvk.Group + "/" + gvk.Kind
	if ns, ok := s.scoped[gk]; ok {
		return ns, nil
	}
	return !clusterScoped[gk], nil
}

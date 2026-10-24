package normalize

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Defaulter fills in the values the API server would default for an object.
// Implementations must not modify their input.
type Defaulter interface {
	Default(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Name() string
}

// NoDefaults is a Defaulter that returns its input unchanged (copied).
type NoDefaults struct{}

// Name implements Defaulter.
func (NoDefaults) Name() string { return "none" }

// Default implements Defaulter.
func (NoDefaults) Default(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return obj.DeepCopy(), nil
}

// Static applies a built-in table of Kubernetes API defaults for core
// workload, networking and storage kinds. The table mirrors the defaulting
// functions of the Kubernetes API types; its agreement with a real API server
// is asserted by the envtest suite (test/e2e). Kinds it does not know are
// returned unchanged.
type Static struct{}

// Name implements Defaulter.
func (Static) Name() string { return "static" }

// Default implements Defaulter.
func (Static) Default(_ context.Context, in *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(in.Object)}
	gvk := u.GroupVersionKind()
	obj := u.Object
	spec := existingMap(obj, "spec")
	switch gvk.Group + "/" + gvk.Kind {
	case "/Pod":
		setDefault(spec, "enableServiceLinks", true)
		podSpec(spec, true)
	case "/PodTemplate":
		podSpec(mapAt(mapAt(obj, "template"), "spec"), true)
	case "/ReplicationController":
		replicationController(obj, spec)
	case "apps/Deployment":
		deployment(spec)
	case "apps/ReplicaSet":
		setDefault(spec, "replicas", int64(1))
		podSpec(templateSpec(spec), true)
	case "apps/StatefulSet":
		statefulSet(spec)
	case "apps/DaemonSet":
		daemonSet(spec)
	case "batch/Job":
		job(spec)
	case "batch/CronJob":
		cronJob(spec)
	case "/Service":
		service(spec)
	case "/PersistentVolumeClaim":
		setDefault(spec, "volumeMode", "Filesystem")
	case "/Secret":
		setDefault(obj, "type", "Opaque")
	case "networking.k8s.io/NetworkPolicy":
		networkPolicy(spec)
	}
	return u, nil
}

// mapAt returns m[key] as a map, creating it when absent. A nil m yields nil.
func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	if _, exists := m[key]; exists {
		return nil // present but not a map: leave malformed input alone
	}
	v := map[string]any{}
	m[key] = v
	return v
}

// existingMap returns m[key] as a map without creating it.
func existingMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func setDefault(m map[string]any, key string, v any) {
	if m == nil {
		return
	}
	if _, ok := m[key]; !ok {
		m[key] = v
	}
}

func eachMap(m map[string]any, key string, fn func(map[string]any)) {
	if m == nil {
		return
	}
	l, _ := m[key].([]any)
	for _, e := range l {
		if em, ok := e.(map[string]any); ok {
			fn(em)
		}
	}
}

func templateSpec(spec map[string]any) map[string]any {
	return mapAt(mapAt(spec, "template"), "spec")
}

// replicationController mirrors core/v1 RC defaulting: selector and object
// labels are copied from the pod template labels when absent.
func replicationController(obj, spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "replicas", int64(1))
	if tl := existingMap(existingMap(existingMap(spec, "template"), "metadata"), "labels"); len(tl) > 0 {
		if _, ok := spec["selector"]; !ok {
			spec["selector"] = runtime.DeepCopyJSONValue(tl)
		}
		if md := existingMap(obj, "metadata"); md != nil {
			if _, ok := md["labels"]; !ok {
				md["labels"] = runtime.DeepCopyJSONValue(tl)
			}
		}
	}
	podSpec(templateSpec(spec), true)
}

func deployment(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "replicas", int64(1))
	setDefault(spec, "revisionHistoryLimit", int64(10))
	setDefault(spec, "progressDeadlineSeconds", int64(600))
	strategy := mapAt(spec, "strategy")
	setDefault(strategy, "type", "RollingUpdate")
	if strategy != nil && strategy["type"] == "RollingUpdate" {
		ru := mapAt(strategy, "rollingUpdate")
		setDefault(ru, "maxUnavailable", "25%")
		setDefault(ru, "maxSurge", "25%")
	}
	podSpec(templateSpec(spec), true)
}

func statefulSet(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "replicas", int64(1))
	setDefault(spec, "podManagementPolicy", "OrderedReady")
	setDefault(spec, "revisionHistoryLimit", int64(10))
	us := mapAt(spec, "updateStrategy")
	setDefault(us, "type", "RollingUpdate")
	if us != nil && us["type"] == "RollingUpdate" {
		ru := mapAt(us, "rollingUpdate")
		setDefault(ru, "partition", int64(0))
		setDefault(ru, "maxUnavailable", int64(1))
	}
	rp := mapAt(spec, "persistentVolumeClaimRetentionPolicy")
	setDefault(rp, "whenDeleted", "Retain")
	setDefault(rp, "whenScaled", "Retain")
	eachMap(spec, "volumeClaimTemplates", func(t map[string]any) {
		setDefault(t, "apiVersion", "v1")
		setDefault(t, "kind", "PersistentVolumeClaim")
		setDefault(mapAt(t, "spec"), "volumeMode", "Filesystem")
	})
	podSpec(templateSpec(spec), true)
}

func daemonSet(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "revisionHistoryLimit", int64(10))
	us := mapAt(spec, "updateStrategy")
	setDefault(us, "type", "RollingUpdate")
	if us != nil && us["type"] == "RollingUpdate" {
		ru := mapAt(us, "rollingUpdate")
		setDefault(ru, "maxUnavailable", int64(1))
		setDefault(ru, "maxSurge", int64(0))
	}
	podSpec(templateSpec(spec), true)
}

func job(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "parallelism", int64(1))
	setDefault(spec, "backoffLimit", int64(6))
	setDefault(spec, "completionMode", "NonIndexed")
	setDefault(spec, "suspend", false)
	setDefault(spec, "manualSelector", false)
	if _, ok := spec["completions"]; !ok {
		if p, _ := spec["parallelism"].(int64); p == 1 {
			spec["completions"] = int64(1)
		}
	}
	if _, ok := spec["podFailurePolicy"]; !ok {
		setDefault(spec, "podReplacementPolicy", "TerminatingOrFailed")
	}
	podSpec(templateSpec(spec), false)
}

func cronJob(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "concurrencyPolicy", "Allow")
	setDefault(spec, "suspend", false)
	setDefault(spec, "successfulJobsHistoryLimit", int64(3))
	setDefault(spec, "failedJobsHistoryLimit", int64(1))
	jt := existingMap(existingMap(spec, "jobTemplate"), "spec")
	podSpec(existingMap(existingMap(jt, "template"), "spec"), false)
}

func service(spec map[string]any) {
	if spec == nil {
		return
	}
	setDefault(spec, "type", "ClusterIP")
	setDefault(spec, "sessionAffinity", "None")
	typ, _ := spec["type"].(string)
	if typ == "ExternalName" {
		return
	}
	setDefault(spec, "internalTrafficPolicy", "Cluster")
	if typ == "NodePort" || typ == "LoadBalancer" {
		setDefault(spec, "externalTrafficPolicy", "Cluster")
	}
	if typ == "LoadBalancer" {
		setDefault(spec, "allocateLoadBalancerNodePorts", true)
	}
	eachMap(spec, "ports", func(p map[string]any) {
		setDefault(p, "protocol", "TCP")
		if port, ok := p["port"]; ok {
			setDefault(p, "targetPort", port)
		}
	})
}

func networkPolicy(spec map[string]any) {
	if spec == nil {
		return
	}
	if _, ok := spec["policyTypes"]; !ok {
		types := []any{"Ingress"}
		if _, egress := spec["egress"]; egress {
			types = append(types, "Egress")
		}
		spec["policyTypes"] = types
	}
	for _, dir := range []string{"ingress", "egress"} {
		eachMap(spec, dir, func(rule map[string]any) {
			eachMap(rule, "ports", func(p map[string]any) { setDefault(p, "protocol", "TCP") })
		})
	}
}

// podSpec applies core/v1 PodSpec, Container and Volume defaults.
// defaultRestart is false for Job templates, where restartPolicy is required.
func podSpec(ps map[string]any, defaultRestart bool) {
	if ps == nil {
		return
	}
	if defaultRestart {
		setDefault(ps, "restartPolicy", "Always")
	}
	setDefault(ps, "dnsPolicy", "ClusterFirst")
	setDefault(ps, "terminationGracePeriodSeconds", int64(30))
	setDefault(ps, "schedulerName", "default-scheduler")
	setDefault(ps, "securityContext", map[string]any{})
	for _, key := range []string{"initContainers", "containers", "ephemeralContainers"} {
		eachMap(ps, key, container)
	}
	eachMap(ps, "volumes", volume)
}

func container(c map[string]any) {
	setDefault(c, "terminationMessagePath", "/dev/termination-log")
	setDefault(c, "terminationMessagePolicy", "File")
	if img, ok := c["image"].(string); ok {
		setDefault(c, "imagePullPolicy", pullPolicy(img))
	}
	setDefault(c, "resources", map[string]any{})
	eachMap(c, "ports", func(p map[string]any) { setDefault(p, "protocol", "TCP") })
	for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
		if pr := existingMap(c, probe); pr != nil {
			setDefault(pr, "timeoutSeconds", int64(1))
			setDefault(pr, "periodSeconds", int64(10))
			setDefault(pr, "successThreshold", int64(1))
			setDefault(pr, "failureThreshold", int64(3))
			handler(pr)
		}
	}
	if lc := existingMap(c, "lifecycle"); lc != nil {
		handler(existingMap(lc, "postStart"))
		handler(existingMap(lc, "preStop"))
	}
	eachMap(c, "env", func(e map[string]any) {
		fieldRef(existingMap(existingMap(e, "valueFrom"), "fieldRef"))
	})
}

func handler(h map[string]any) {
	if g := existingMap(h, "httpGet"); g != nil {
		setDefault(g, "scheme", "HTTP")
	}
	if g := existingMap(h, "grpc"); g != nil {
		setDefault(g, "service", "")
	}
}

func fieldRef(fr map[string]any) {
	setDefault(fr, "apiVersion", "v1")
}

func volume(v map[string]any) {
	for _, src := range []string{"configMap", "secret", "projected", "downwardAPI"} {
		if s := existingMap(v, src); s != nil {
			setDefault(s, "defaultMode", int64(420))
			if src == "downwardAPI" {
				eachMap(s, "items", func(it map[string]any) { fieldRef(existingMap(it, "fieldRef")) })
			}
		}
	}
	if hp := existingMap(v, "hostPath"); hp != nil {
		setDefault(hp, "type", "")
	}
}

// pullPolicy mirrors the core/v1 defaulting rule: images tagged ":latest"
// or untagged (and not pinned by digest) default to Always.
func pullPolicy(image string) string {
	if strings.Contains(image, "@") {
		return "IfNotPresent"
	}
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		if name[i+1:] == "latest" {
			return "Always"
		}
		return "IfNotPresent"
	}
	return "Always"
}

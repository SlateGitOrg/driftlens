package normalize

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"
)

func obj(t *testing.T, y string) *unstructured.Unstructured {
	t.Helper()
	js, err := yaml.YAMLToJSON([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := utiljson.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

func TestCanonicalize(t *testing.T) {
	in := map[string]any{
		"a":        nil,
		"empty":    []any{},
		"keepMap":  map[string]any{},
		"labels":   map[string]any{},
		"n":        float64(3),
		"frac":     1.5,
		"i":        int(2),
		"i32":      int32(4),
		"huge":     float64(1 << 60),
		"list":     []any{nil, float64(1)},
		"metadata": map[string]any{"creationTimestamp": nil},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "1000m", "memory": "1024Mi", "bad": "lots"},
			"limits":   map[string]any{"cpu": "0.5"},
		},
		"emptyDir": map[string]any{"sizeLimit": "1024Mi"},
		"other":    "1000m",
	}
	Canonicalize(in)
	want := map[string]any{
		"keepMap": map[string]any{},
		"n":       int64(3),
		"frac":    1.5,
		"i":       int64(2),
		"i32":     int64(4),
		"huge":    float64(1 << 60),
		"list":    []any{int64(1)},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "1", "memory": "1Gi", "bad": "lots"},
			"limits":   map[string]any{"cpu": "500m"},
		},
		"emptyDir": map[string]any{"sizeLimit": "1Gi"},
		"other":    "1000m",
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("got  %#v\nwant %#v", in, want)
	}
}

func TestStripServerFieldsAndAssigned(t *testing.T) {
	u := obj(t, `
apiVersion: v1
kind: Service
metadata:
  name: s
  uid: x
  resourceVersion: "9"
  managedFields: [{}]
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
    keep: me
spec:
  clusterIP: 10.0.0.1
  clusterIPs: [10.0.0.1]
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  ports: [{port: 80, nodePort: 30000}]
status: {loadBalancer: {}}
`)
	StripServerFields(u.Object)
	StripAssigned("", "Service", u.Object, nil)
	if _, ok := u.Object["status"]; ok {
		t.Fatal("status kept")
	}
	md := u.Object["metadata"].(map[string]any)
	if _, ok := md["uid"]; ok {
		t.Fatal("uid kept")
	}
	if ann := md["annotations"].(map[string]any); len(ann) != 1 || ann["keep"] != "me" {
		t.Fatalf("annotations %v", ann)
	}
	spec := u.Object["spec"].(map[string]any)
	if len(spec) != 1 {
		t.Fatalf("server-assigned fields kept: %v", spec)
	}
	if port := spec["ports"].([]any)[0].(map[string]any); port["nodePort"] != nil {
		t.Fatal("nodePort kept")
	}

	// A desired object that pins the clusterIP keeps it.
	live := obj(t, "spec: {clusterIP: 10.0.0.1}")
	StripAssigned("", "Service", live.Object, map[string]any{"spec": map[string]any{"clusterIP": "10.0.0.1"}})
	if live.Object["spec"].(map[string]any)["clusterIP"] != "10.0.0.1" {
		t.Fatal("pinned clusterIP removed")
	}
	noMeta := map[string]any{"kind": "X"}
	StripServerFields(noMeta) // must not panic
}

func TestPrepareLive(t *testing.T) {
	u := obj(t, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: c
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"n":"1"}}'
  managedFields:
    - manager: kubectl-client-side-apply
      operation: Update
data: {n: "1"}
`)
	p, err := PrepareLive(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.LastApplied == nil || p.LastApplied.Object["data"].(map[string]any)["n"] != "1" {
		t.Fatalf("last applied: %+v", p.LastApplied)
	}
	if len(p.Managed) != 1 {
		t.Fatal("managedFields not extracted")
	}
	if _, ok := p.Object.Object["metadata"].(map[string]any)["managedFields"]; ok {
		t.Fatal("managedFields not stripped")
	}
	if _, ok := u.Object["metadata"].(map[string]any)["managedFields"]; !ok {
		t.Fatal("input was modified")
	}
	u.SetAnnotations(map[string]string{LastAppliedAnnotation: "{broken"})
	if _, err := PrepareLive(u, nil); err == nil || !strings.Contains(err.Error(), "last-applied") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestPrepareDesiredSecret(t *testing.T) {
	u := obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: s}
data: {a: YQ==, b: b2xk}
stringData: {b: new, c: "3"}
`)
	out := PrepareDesired(u)
	want := map[string]any{"a": "YQ==", "b": "bmV3", "c": "Mw=="}
	if !reflect.DeepEqual(out.Object["data"], want) {
		t.Fatalf("data %v", out.Object["data"])
	}
	if _, ok := out.Object["stringData"]; ok {
		t.Fatal("stringData kept")
	}
	only := obj(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData: {x: why}\n")
	if got := PrepareDesired(only).Object["data"]; !reflect.DeepEqual(got, map[string]any{"x": "d2h5"}) {
		t.Fatalf("data %v", got)
	}
}

func TestDefaultersDoNotMutateInput(t *testing.T) {
	u := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {template: {spec: {containers: [{name: c, image: x}]}}}\n")
	before := u.DeepCopy()
	for _, d := range []Defaulter{Static{}, NoDefaults{}} {
		out, err := d.Default(context.Background(), u)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(u.Object, before.Object) {
			t.Fatalf("%s mutated its input", d.Name())
		}
		if d.Name() == "none" && !reflect.DeepEqual(out.Object, before.Object) {
			t.Fatal("NoDefaults changed the object")
		}
	}
}

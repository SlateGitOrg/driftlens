package objpath

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/value"
)

func deployment() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"example.com/a~b": "x"}},
		"spec": map[string]any{
			"template": map[string]any{"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "web", "image": "nginx:1", "ports": []any{map[string]any{"containerPort": int64(80), "protocol": "TCP"}}},
					map[string]any{"name": "side", "image": "busybox"},
				},
			}},
			"finalizers": []any{"a", "b"},
			"args":       []any{"x", "y"},
		},
	}
}

func TestFromFieldPathAndRendering(t *testing.T) {
	obj := deployment()
	fp := fieldpath.MakePathOrDie("spec", "template", "spec", "containers", fieldpath.KeyByFields("name", "side"), "image")
	p := FromFieldPath(fp, obj)
	if got, want := p.String(), `.spec.template.spec.containers[name="side"].image`; got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
	pointer, err := p.Pointer()
	if err != nil || pointer != "/spec/template/spec/containers/1/image" {
		t.Fatalf("Pointer() = %q, %v", pointer, err)
	}
	if v, ok := Get(obj, p); !ok || v != "busybox" {
		t.Fatalf("Get = %v %v", v, ok)
	}
	if p.Last() != "image" {
		t.Fatalf("Last() = %q", p.Last())
	}

	// Composite keys render sorted and quoted.
	ports := fieldpath.MakePathOrDie("spec", "template", "spec", "containers", fieldpath.KeyByFields("name", "web"),
		"ports", fieldpath.KeyByFields("protocol", "TCP", "containerPort", 80))
	pp := FromFieldPath(ports, obj)
	if got, want := pp.String(), `.spec.template.spec.containers[name="web"].ports[containerPort=80,protocol="TCP"]`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if pp2, _ := pp.Pointer(); pp2 != "/spec/template/spec/containers/0/ports/0" {
		t.Fatalf("ports pointer %s", pp2)
	}

	// Set elements and indices.
	v := value.NewValueInterface("b")
	set := FromFieldPath(fieldpath.Path{fieldpath.PathElement{FieldName: ptr("spec")}, fieldpath.PathElement{FieldName: ptr("finalizers")}, fieldpath.PathElement{Value: &v}}, obj)
	if set.String() != `.spec.finalizers[="b"]` {
		t.Fatalf("set path %s", set.String())
	}
	if p, _ := set.Pointer(); p != "/spec/finalizers/1" {
		t.Fatalf("set pointer %s", p)
	}
	idx := 1
	ix := FromFieldPath(fieldpath.Path{fieldpath.PathElement{FieldName: ptr("spec")}, fieldpath.PathElement{FieldName: ptr("args")}, fieldpath.PathElement{Index: &idx}}, obj)
	if ix.String() != ".spec.args[1]" {
		t.Fatalf("index path %s", ix.String())
	}
	if v, ok := Get(obj, ix); !ok || v != "y" {
		t.Fatalf("Get index = %v", v)
	}

	// Fields that need quoting, and pointer escaping.
	ann := Fields("metadata", "annotations", "example.com/a~b")
	if ann.String() != `.metadata.annotations["example.com/a~b"]` {
		t.Fatalf("quoted field %s", ann.String())
	}
	if p, _ := ann.Pointer(); p != "/metadata/annotations/example.com~1a~0b" {
		t.Fatalf("escaped pointer %s", p)
	}
	if Path(nil).String() != "." {
		t.Fatal("empty path should render as .")
	}
	if p, err := Path(nil).Pointer(); err != nil || p != "" {
		t.Fatal("empty pointer")
	}
}

func ptr(s string) *string { return &s }

func TestUnresolvedPositions(t *testing.T) {
	fp := fieldpath.MakePathOrDie("spec", "containers", fieldpath.KeyByFields("name", "gone"), "image")
	p := FromFieldPath(fp, map[string]any{"spec": map[string]any{"containers": []any{}}})
	if _, err := p.Pointer(); err == nil {
		t.Fatal("expected error for unknown list position")
	}
	if _, ok := Get(map[string]any{}, p); ok {
		t.Fatal("Get should fail")
	}
	if _, ok := Get("scalar", Fields("a")); ok {
		t.Fatal("Get through scalar should fail")
	}
	if _, ok := Get(map[string]any{"a": []any{1}}, Path{Field("a"), Index(4)}); ok {
		t.Fatal("Get out of range should fail")
	}
	if _, ok := Get(map[string]any{"a": "x"}, Path{Field("a"), {Kind: SegValue, Value: "x"}}); ok {
		t.Fatal("Get set element of scalar should fail")
	}
}

func TestHasPrefix(t *testing.T) {
	a := Path{Field("spec"), Key(KeyField{"name", "web"}), Field("image")}
	b := Path{Field("spec"), Key(KeyField{"name", "web"})}
	c := Path{Field("spec"), Key(KeyField{"name", "side"})}
	if !a.HasPrefix(b) || a.HasPrefix(c) || b.HasPrefix(a) {
		t.Fatal("HasPrefix keyed")
	}
	if !(Path{Index(2)}).HasPrefix(Path{Index(2)}) || (Path{Index(2)}).HasPrefix(Path{Index(3)}) {
		t.Fatal("HasPrefix index")
	}
	sv := Path{{Kind: SegValue, Value: "x"}}
	if !sv.HasPrefix(sv) || sv.HasPrefix(Path{{Kind: SegValue, Value: "y"}}) || sv.HasPrefix(Path{Field("x")}) {
		t.Fatal("HasPrefix set")
	}
	if (Path{Key(KeyField{"a", 1})}).HasPrefix(Path{Key(KeyField{"a", 1}, KeyField{"b", 2})}) {
		t.Fatal("key arity")
	}
	if (Path{Index(1)}).Last() != "" {
		t.Fatal("Last of index")
	}
}

func TestScalarEqual(t *testing.T) {
	cases := []struct {
		a, b any
		eq   bool
	}{
		{int64(3), float64(3), true},
		{int32(3), int(3), true},
		{float32(1.5), float64(1.5), true},
		{int64(3), "3", false},
		{"a", "a", true},
		{nil, nil, true},
		{true, false, false},
	}
	for _, c := range cases {
		if got := ScalarEqual(c.a, c.b); got != c.eq {
			t.Errorf("ScalarEqual(%v,%v)=%v", c.a, c.b, got)
		}
	}
}

func TestDelete(t *testing.T) {
	obj := func() map[string]any {
		return map[string]any{"spec": map[string]any{
			"replicas":   int64(1),
			"finalizers": []any{"a", "b"},
			"containers": []any{
				map[string]any{"name": "web", "image": "w", "args": []any{"x", "y", "z"}},
				map[string]any{"name": "side", "image": "s"},
			},
		}}
	}
	web := Key(KeyField{Name: "name", Value: "web"})
	cases := []struct {
		path Path
		ok   bool
		want string
	}{
		{Fields("spec", "replicas"), true, `"replicas":`},
		{append(Fields("spec", "containers"), web, Field("image")), true, `"image":"w"`},
		{append(Fields("spec", "containers"), Key(KeyField{Name: "name", Value: "side"})), true, `"side"`},
		{append(Fields("spec", "containers"), Index(0), Field("args"), Index(1)), true, `"y"`},
		{append(Fields("spec", "finalizers"), Segment{Kind: SegValue, Value: "a"}), true, `"a"`},
		{append(Fields("spec", "containers"), Key(KeyField{Name: "name", Value: "nope"})), false, ""},
		{append(Fields("spec", "containers"), Index(5)), false, ""},
		{append(Fields("spec", "replicas"), Field("x")), false, ""},
		{append(Fields("spec", "replicas"), Index(0)), false, ""},
		{Fields("spec", "missing", "x"), false, ""},
		{nil, false, ""},
	}
	for _, c := range cases {
		o := obj()
		if got := Delete(o, c.path); got != c.ok {
			t.Errorf("Delete(%s) = %v", c.path, got)
			continue
		}
		b, _ := json.Marshal(o)
		if c.ok && strings.Contains(string(b), c.want) {
			t.Errorf("Delete(%s) left %s in %s", c.path, c.want, b)
		}
		if !c.ok && !reflect.DeepEqual(o, obj()) {
			t.Errorf("Delete(%s) changed the object: %s", c.path, b)
		}
	}
	o := obj()
	Delete(o, append(Fields("spec", "containers"), web))
	if cs := o["spec"].(map[string]any)["containers"].([]any); len(cs) != 1 || cs[0].(map[string]any)["name"] != "side" {
		t.Fatalf("list not compacted: %v", cs)
	}
}

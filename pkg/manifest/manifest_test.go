package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func names(t *testing.T, objs []*unstructured.Unstructured) []string {
	t.Helper()
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.GetName()
	}
	return out
}

func TestDecode(t *testing.T) {
	in := `
---
# only a comment
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec: {replicas: 3, ratio: 0.5}
---
null
`
	objs, err := Decode(strings.NewReader(in), "in")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects", len(objs))
	}
	spec := objs[0].Object["spec"].(map[string]any)
	if _, ok := spec["replicas"].(int64); !ok {
		t.Fatalf("replicas decoded as %T, want int64", spec["replicas"])
	}
	if _, ok := spec["ratio"].(float64); !ok {
		t.Fatalf("ratio decoded as %T", spec["ratio"])
	}
	r := RefOf(objs[0])
	if r.String() != "apps/v1 Deployment shop/web" || r.Key() != "apps/Deployment/shop/web" {
		t.Fatalf("ref %s / %s", r, r.Key())
	}
	if r.GVK().Kind != "Deployment" {
		t.Fatal("GVK")
	}
}

func TestDecodeErrors(t *testing.T) {
	for name, in := range map[string]string{
		"no apiVersion": "kind: X\nmetadata: {name: a}\n",
		"no kind":       "apiVersion: v1\nmetadata: {name: a}\n",
		"no name":       "apiVersion: v1\nkind: ConfigMap\nmetadata: {generateName: a-}\n",
		"bad gv":        "apiVersion: a/b/c\nkind: X\nmetadata: {name: a}\n",
		"bad yaml":      "apiVersion: [v1\n",
		"not object":    "- a\n- b\n",
		"bad item":      "apiVersion: v1\nkind: List\nitems: [1]\n",
		"bad nested":    "apiVersion: v1\nkind: List\nitems: [{kind: X}]\n",
	} {
		if _, err := Decode(strings.NewReader(in), name); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadDirectoryFileAndStdin(t *testing.T) {
	objs, err := Load([]string{"testdata/dir", "-"}, strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata: {name: ns}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, objs)
	want := []string{"a", "b", "c", "ns"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := Load([]string{"testdata/missing.yaml"}, nil); err == nil {
		t.Fatal("missing file should fail")
	}
}

func TestKustomize(t *testing.T) {
	if !IsKustomization("testdata/kust") || IsKustomization("testdata/dir") {
		t.Fatal("IsKustomization")
	}
	objs, err := Load([]string{"testdata/kust"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetNamespace() != "team" || objs[0].GetLabels()["owner"] != "team" {
		t.Fatalf("kustomize output wrong: %+v", objs[0].Object)
	}
	if _, err := Load([]string{"testdata/withkust"}, nil); err == nil || !strings.Contains(err.Error(), "pass it directly with -k") {
		t.Fatalf("nested kustomization should be rejected, got %v", err)
	}
	if _, err := BuildKustomization("testdata/dir"); err == nil {
		t.Fatal("building a non-kustomization should fail")
	}
}

func TestEncodeListRoundTripAndDedupe(t *testing.T) {
	objs, err := Load([]string{"testdata/dir"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeList(objs)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(bytes.NewReader(b), "roundtrip")
	if err != nil || len(back) != len(objs) {
		t.Fatalf("roundtrip: %v, %d objects", err, len(back))
	}
	dupd, dups := Dedupe(append(objs, back[0]))
	if len(dupd) != len(objs) || len(dups) != 1 || dups[0].Name != "a" {
		t.Fatalf("dedupe: %d kept, dups %v", len(dupd), dups)
	}
}

func TestRefOrdering(t *testing.T) {
	a := Ref{Group: "", Kind: "ConfigMap", Namespace: "a", Name: "x"}
	cases := []struct {
		b    Ref
		less bool
	}{
		{Ref{Group: "apps", Kind: "ConfigMap"}, true},
		{Ref{Kind: "Secret"}, true},
		{Ref{Kind: "ConfigMap", Namespace: "b"}, true},
		{Ref{Kind: "ConfigMap", Namespace: "a", Name: "y"}, true},
		{Ref{Kind: "ConfigMap", Namespace: "a", Name: "w"}, false},
	}
	for _, c := range cases {
		if a.Less(c.b) != c.less {
			t.Errorf("%v < %v should be %v", a, c.b, c.less)
		}
	}
	if (Ref{Version: "v1", Kind: "Namespace", Name: "n"}).String() != "v1 Namespace n" {
		t.Fatal("cluster-scoped String")
	}
}

func TestLoadUnreadableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("kind: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{dir}, nil); err == nil {
		t.Fatal("bad file in directory should fail")
	}
}

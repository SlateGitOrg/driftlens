package drift

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"

	"github.com/archit2501/driftlens/pkg/manifest"
	"github.com/archit2501/driftlens/pkg/normalize"
	"github.com/archit2501/driftlens/pkg/objpath"
	"github.com/archit2501/driftlens/pkg/rules"
)

func u(t *testing.T, y string) *unstructured.Unstructured {
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

const cm = `
apiVersion: v1
kind: ConfigMap
metadata: {name: c, namespace: ns}
data: {a: "1", b: "2"}
`

func TestNoHistoryIsTwoWay(t *testing.T) {
	e := New(Options{})
	live := u(t, cm)
	live.Object["data"] = map[string]any{"a": "9", "extra": "x"}
	res := e.Diff(context.Background(), u(t, cm), live)
	if res.Basis != BasisNone || res.Status != StatusDrifted {
		t.Fatalf("basis %s status %s", res.Basis, res.Status)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("changes: %+v", res.Changes)
	}
	for _, c := range res.Changes {
		if c.Origin != OriginUnknown {
			t.Fatalf("origin %s", c.Origin)
		}
	}
	// Without attribution, the live-only key is treated as a default...
	if len(res.Ignored) != 1 || res.Ignored[0].Path != ".data.extra" {
		t.Fatalf("ignored: %+v", res.Ignored)
	}
	// ...unless the configuration asks to report it.
	cfg := rules.Default()
	cfg.ReportUnattributed = true
	res = New(Options{Config: cfg}).Diff(context.Background(), u(t, cm), live)
	if len(res.Changes) != 3 || len(res.Ignored) != 0 {
		t.Fatalf("reportUnattributed: %+v", res.Changes)
	}
}

func TestErrorsAndMissing(t *testing.T) {
	e := New(Options{})
	ctx := context.Background()
	if res := e.Diff(ctx, u(t, cm), nil); res.Status != StatusMissing {
		t.Fatal("missing")
	}
	other := u(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: c, namespace: ns}\n")
	if res := e.Diff(ctx, u(t, cm), other); res.Status != StatusError || !strings.Contains(res.Error, "live object is") {
		t.Fatalf("kind mismatch: %+v", res)
	}
	v2 := u(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: c}\n")
	v1b := u(t, "apiVersion: apps/v1beta1\nkind: Deployment\nmetadata: {name: c}\n")
	if res := e.Diff(ctx, v2, v1b); res.Status != StatusError || !strings.Contains(res.Error, "version") {
		t.Fatalf("version mismatch: %+v", res)
	}
	bad := u(t, cm)
	bad.SetAnnotations(map[string]string{normalize.LastAppliedAnnotation: "{"})
	if res := e.Diff(ctx, u(t, cm), bad); res.Status != StatusError {
		t.Fatalf("bad annotation: %+v", res)
	}
	mf := u(t, cm)
	mf.Object["metadata"].(map[string]any)["managedFields"] = []any{"junk"}
	if res := e.Diff(ctx, u(t, cm), mf); res.Status != StatusInSync || len(res.Warnings) != 1 {
		t.Fatalf("bad managedFields should warn: %+v", res)
	}
}

type failingDefaulter struct{}

func (failingDefaulter) Name() string { return "fail" }
func (failingDefaulter) Default(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, errors.New("boom")
}

func TestDefaulterFailure(t *testing.T) {
	res := New(Options{Defaulter: failingDefaulter{}}).Diff(context.Background(), u(t, cm), u(t, cm))
	if res.Status != StatusError || !strings.Contains(res.Error, "boom") {
		t.Fatalf("%+v", res)
	}
	live := u(t, cm)
	live.SetAnnotations(map[string]string{normalize.LastAppliedAnnotation: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`})
	calls := 0
	res = New(Options{Defaulter: &countingDefaulter{fail: 2, calls: &calls}}).Diff(context.Background(), u(t, cm), live)
	if res.Status != StatusError || !strings.Contains(res.Error, "last-applied") {
		t.Fatalf("%+v", res)
	}
}

type countingDefaulter struct {
	fail  int
	calls *int
}

func (c *countingDefaulter) Name() string { return "count" }
func (c *countingDefaulter) Default(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	*c.calls++
	if *c.calls == c.fail {
		return nil, errors.New("second call fails")
	}
	return o.DeepCopy(), nil
}

func TestSchemaFallback(t *testing.T) {
	d := u(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {replicaz: 3}\n")
	l := u(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {replicaz: 4}\n")
	res := New(Options{Defaulter: normalize.NoDefaults{}}).Diff(context.Background(), d, l)
	if res.Schema != schemaDeduced || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "replicaz") {
		t.Fatalf("%+v", res)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != ".spec.replicaz" {
		t.Fatalf("%+v", res.Changes)
	}
}

type fakeSource struct {
	objs map[string]*unstructured.Unstructured
	err  error
	nsEr error
}

func (f fakeSource) Get(_ context.Context, r manifest.Ref) (*unstructured.Unstructured, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.objs[r.Key()], nil
}

func (f fakeSource) Namespaced(context.Context, schema.GroupVersionKind) (bool, error) {
	return true, f.nsEr
}

func TestRunAggregates(t *testing.T) {
	ctx := context.Background()
	noNS := u(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {a: \"1\"}\n")
	live := u(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: team}\ndata: {a: \"1\"}\n")
	src := fakeSource{objs: map[string]*unstructured.Unstructured{"/ConfigMap/team/c": live}}
	rep, err := New(Options{DefaultNamespace: "team"}).Run(ctx, []*unstructured.Unstructured{noNS, noNS}, src)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.ByStatus[StatusInSync] != 1 || rep.Summary.ByStatus[StatusError] != 1 {
		t.Fatalf("summary %+v", rep.Summary)
	}
	if !rep.HasDrift() || !rep.HasLiveDrift() {
		t.Fatal("duplicate is an error, which counts as drift")
	}

	rep, _ = New(Options{}).Run(ctx, []*unstructured.Unstructured{noNS}, fakeSource{nsEr: errors.New("no mapping")})
	if rep.Objects[0].Status != StatusError {
		t.Fatal("namespaced error")
	}
	rep, _ = New(Options{}).Run(ctx, []*unstructured.Unstructured{noNS}, fakeSource{err: errors.New("forbidden")})
	if rep.Objects[0].Status != StatusError || rep.Objects[0].Error != "forbidden" {
		t.Fatal("get error")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := New(Options{}).Run(cctx, []*unstructured.Unstructured{noNS}, src); err == nil {
		t.Fatal("cancelled context")
	}
	empty := &Report{}
	empty.Summarize()
	if empty.HasDrift() || empty.HasLiveDrift() {
		t.Fatal("empty report has no drift")
	}
	outOfSync := &Report{Objects: []ObjectResult{{Status: StatusOutOfSync}}}
	if !outOfSync.HasDrift() || outOfSync.HasLiveDrift() {
		t.Fatal("OutOfSync is drift but not live drift")
	}
}

func TestSecretValuesAreRedacted(t *testing.T) {
	d := u(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData: {password: hunter2}\n")
	l := u(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata: {password: b2xk}\ntype: Opaque\n")
	res := New(Options{}).Diff(context.Background(), d, l)
	if len(res.Changes) != 1 {
		t.Fatalf("%+v", res.Changes)
	}
	c := res.Changes[0]
	if c.Desired != Redacted || c.Live != Redacted {
		t.Fatalf("secret leaked: %+v", c)
	}
}

func TestCategory(t *testing.T) {
	deploy := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	key := objpath.Key(objpath.KeyField{Name: "name", Value: "c"})
	cases := []struct {
		gvk  schema.GroupVersionKind
		p    objpath.Path
		want Category
	}{
		{deploy, objpath.Fields("spec", "replicas"), CategoryScale},
		{schema.GroupVersionKind{Kind: "ConfigMap"}, objpath.Fields("spec", "replicas"), CategorySpec},
		{deploy, objpath.Path{objpath.Field("spec"), objpath.Field("template"), objpath.Field("spec"), objpath.Field("initContainers"), key, objpath.Field("image")}, CategoryImage},
		{deploy, objpath.Path{objpath.Field("spec"), objpath.Field("containers"), key, objpath.Field("image")}, CategoryImage},
		{deploy, objpath.Path{objpath.Field("spec"), objpath.Field("volumes"), key, objpath.Field("image")}, CategorySpec},
		{deploy, objpath.Fields("image"), CategorySpec},
		{deploy, objpath.Fields("metadata", "labels", "a"), CategoryMetadata},
	}
	for _, c := range cases {
		if got := category(c.gvk, c.p, owners{}); got != c.want {
			t.Errorf("%s: %s, want %s", c.p, got, c.want)
		}
	}
}

func TestEqual(t *testing.T) {
	a := map[string]any{"x": []any{int64(1), map[string]any{"y": "z"}}}
	b := map[string]any{"x": []any{float64(1), map[string]any{"y": "z"}}}
	if !equal(a, b) {
		t.Fatal("numeric equality inside structures")
	}
	for _, other := range []any{
		map[string]any{"x": []any{int64(1)}},
		map[string]any{"q": []any{int64(1), map[string]any{"y": "z"}}},
		map[string]any{"x": []any{int64(2), map[string]any{"y": "z"}}},
		map[string]any{"x": "s"},
		"s",
	} {
		if equal(a, other) {
			t.Errorf("equal(%v, %v)", a, other)
		}
	}
	if equal([]any{1}, "x") || !equal(true, true) {
		t.Fatal("scalars")
	}
}

func TestSnapshotSourceScope(t *testing.T) {
	ns := u(t, "apiVersion: v1\nkind: Namespace\nmetadata: {name: a}\n")
	widget := u(t, "apiVersion: example.com/v1\nkind: Widget\nmetadata: {name: w}\n")
	s := NewSnapshotSource([]*unstructured.Unstructured{ns, widget})
	ctx := context.Background()
	for gvk, want := range map[schema.GroupVersionKind]bool{
		{Version: "v1", Kind: "Namespace"}:                                       false,
		{Group: "example.com", Version: "v1", Kind: "Widget"}:                    false,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}: false,
		{Version: "v1", Kind: "ConfigMap"}:                                       true,
	} {
		if got, _ := s.Namespaced(ctx, gvk); got != want {
			t.Errorf("%s namespaced = %v", gvk, got)
		}
	}
	if o, _ := s.Get(ctx, manifest.Ref{Kind: "Namespace", Name: "a"}); o == nil {
		t.Fatal("get")
	}
	if o, _ := s.Get(ctx, manifest.Ref{Kind: "Namespace", Name: "b"}); o != nil {
		t.Fatal("get missing")
	}
}

func TestIgnoredKeyLeavesNoEmptyMapDrift(t *testing.T) {
	cfg, err := rules.Parse([]byte("apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{name: stamp, paths: ['.metadata.annotations[\"x/synced\"]']}, {name: empty-list, paths: [/data/drop]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	live := u(t, cm)
	live.SetAnnotations(map[string]string{"x/synced": "now"})
	live.Object["metadata"].(map[string]any)["managedFields"] = []any{map[string]any{
		"manager": "sync-bot", "operation": "Update",
		"fieldsV1": map[string]any{"f:metadata": map[string]any{"f:annotations": map[string]any{".": map[string]any{}, "f:x/synced": map[string]any{}}}},
	}}
	res := New(Options{Config: cfg}).Diff(context.Background(), u(t, cm), live)
	if res.Status != StatusInSync || len(res.Ignored) != 1 || res.Ignored[0].Rule != "stamp" {
		t.Fatalf("%+v", res)
	}
	// An empty container on the desired side alone is not drift either.
	d := u(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: ns, annotations: {x/synced: t}}\ndata: {a: \"1\", b: \"2\"}\n")
	res = New(Options{Config: cfg}).Diff(context.Background(), d, u(t, cm))
	if res.Status != StatusInSync || len(res.Ignored) != 1 {
		t.Fatalf("%+v", res)
	}
}

package drift

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/archit2501/driftlens/pkg/normalize"
	"github.com/archit2501/driftlens/pkg/rules"
)

const twoContainers = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: d, namespace: ns}
spec:
  selector: {matchLabels: {app: d}}
  template:
    metadata: {labels: {app: d}}
    spec:
      containers:
      - {name: web, image: "nginx:1"}
      - {name: side, image: "busybox:1"}
`

// defaulted returns obj as the API server would store it (static defaults).
func defaulted(t *testing.T, obj map[string]any) *unstructured.Unstructured {
	t.Helper()
	out, err := normalize.Static{}.Default(context.Background(), &unstructured.Unstructured{Object: obj})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// reorder swaps the two containers of a twoContainers object and sets the
// image of the one named name.
func reorder(t *testing.T, name, image string) map[string]any {
	t.Helper()
	live := u(t, twoContainers)
	spec := live.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	cs := spec["containers"].([]any)
	cs[0], cs[1] = cs[1], cs[0]
	for _, c := range cs {
		if c.(map[string]any)["name"] == name {
			c.(map[string]any)["image"] = image
		}
	}
	return live.Object
}

func changeLines(res ObjectResult) []string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, string(c.Op)+" "+c.Path+" "+string(c.Origin))
	}
	return out
}

// Index steps into associative lists count positions in the desired object
// and are matched on the other sides by merge key, so a live reorder does
// not make the rule hide a different element on each side.
func TestPositionalIgnoreFollowsMergeKeys(t *testing.T) {
	for _, pat := range []string{"/spec/template/spec/containers/1/image", ".spec.template.spec.containers[1].image"} {
		t.Run(pat, func(t *testing.T) {
			cfg, err := rules.Parse([]byte("apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{name: side-image, paths: ['" + pat + "']}]\n"))
			if err != nil {
				t.Fatal(err)
			}
			e := New(Options{Config: cfg})
			res := e.Diff(context.Background(), u(t, twoContainers), defaulted(t, reorder(t, "web", "nginx:2")))
			got := changeLines(res)
			if len(got) != 1 || got[0] != `modify .spec.template.spec.containers[name="web"].image unknown` {
				t.Fatalf("changes: %q", got)
			}
			if len(res.Ignored) != 0 {
				t.Fatalf("ignored: %+v", res.Ignored)
			}

			// The rule still hides the element it names: side's image
			// changed live (and reordered) is ignored and attributed.
			res = e.Diff(context.Background(), u(t, twoContainers), defaulted(t, reorder(t, "side", "busybox:2")))
			if res.Status != StatusInSync || len(res.Ignored) != 1 || res.Ignored[0].Rule != "side-image" ||
				!strings.Contains(res.Ignored[0].Path, `[name="side"].image`) {
				t.Fatalf("changes %q ignored %+v", changeLines(res), res.Ignored)
			}
		})
	}
}

// Elements of atomic lists (here: args, and every list under the deduced
// schema) are compared by position, so the rule keeps pruning by position
// on each side.
func TestPositionalIgnoreInAtomicList(t *testing.T) {
	cfg, err := rules.Parse([]byte("apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{name: arg1, paths: ['.spec.template.spec.containers[name=web].args[1]']}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := u(t, twoContainers)
	l := u(t, twoContainers)
	set := func(o map[string]any, args ...any) {
		c := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		c["args"] = args
	}
	set(d.Object, "a", "b", "c")
	set(l.Object, "a", "B", "c")
	res := New(Options{Config: cfg}).Diff(context.Background(), d, defaulted(t, l.Object))
	if res.Status != StatusInSync {
		t.Fatalf("changes %q", changeLines(res))
	}
	set(l.Object, "A", "B", "c")
	res = New(Options{Config: cfg}).Diff(context.Background(), d, defaulted(t, l.Object))
	if got := changeLines(res); len(got) != 1 || !strings.Contains(got[0], `[name="web"].args`) {
		t.Fatalf("changes %q", got)
	}
}

func containers(o map[string]any) []any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
}

func setContainers(o map[string]any, cs []any) {
	o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"] = cs
}

func ignoreCfg(t *testing.T, pats ...string) *rules.Config {
	t.Helper()
	y := "apiVersion: driftlens/v1\nkind: DriftConfig\nignore:\n"
	for i, p := range pats {
		y += "- {name: r" + string(rune('0'+i)) + ", paths: ['" + p + "']}\n"
	}
	cfg, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A container inserted at index 0 live (kubectl edit, a JSON patch) shifts
// every live position; the rule must still hide desired's second container
// and nothing else, and the inserted container is reported.
func TestPositionalIgnoreAfterLiveInsert(t *testing.T) {
	cfg := ignoreCfg(t, "/spec/template/spec/containers/1/image")
	cfg.ReportUnattributed = true // no history: report the live-only container
	e := New(Options{Config: cfg})
	l := u(t, twoContainers).Object
	cs := containers(l)
	cs[1].(map[string]any)["image"] = "busybox:2" // hidden by the rule
	setContainers(l, append([]any{map[string]any{"name": "debug", "image": "alpine:3"}}, cs...))
	res := e.Diff(context.Background(), u(t, twoContainers), defaulted(t, l))
	got := changeLines(res)
	if len(got) != 1 || got[0] != `remove .spec.template.spec.containers[name="debug"] unknown` {
		t.Fatalf("changes %q", got)
	}
	if len(res.Ignored) != 1 || res.Ignored[0].Rule != "r0" || !strings.Contains(res.Ignored[0].Path, `[name="side"].image`) {
		t.Fatalf("ignored %+v", res.Ignored)
	}
}

// Nested associative lists (env inside containers) are resolved level by
// level, and the last-applied side is pruned by key as well.
func TestPositionalIgnoreNestedAndLastApplied(t *testing.T) {
	const withEnv = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: d, namespace: ns}
spec:
  selector: {matchLabels: {app: d}}
  template:
    metadata: {labels: {app: d}}
    spec:
      containers:
      - name: web
        image: "nginx:1"
        env: [{name: A, value: "1"}, {name: B, value: "2"}]
`
	e := New(Options{Config: ignoreCfg(t, ".spec.template.spec.containers[0].env[1].value")})
	l := u(t, withEnv)
	ann, err := json.Marshal(u(t, withEnv).Object)
	if err != nil {
		t.Fatal(err)
	}
	env := containers(l.Object)[0].(map[string]any)
	// Live reorders env and changes both values; only B is ignored.
	env["env"] = []any{map[string]any{"name": "B", "value": "20"}, map[string]any{"name": "A", "value": "10"}}
	l.SetAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": string(ann)})
	res := e.Diff(context.Background(), u(t, withEnv), defaulted(t, l.Object))
	if res.Basis != BasisLastApplied {
		t.Fatalf("basis %s", res.Basis)
	}
	got := changeLines(res)
	if len(got) != 1 || got[0] != `modify .spec.template.spec.containers[name="web"].env[name="A"].value live` {
		t.Fatalf("changes %q", got)
	}
	if len(res.Ignored) != 1 || !strings.Contains(res.Ignored[0].Path, `env[name="B"].value`) {
		t.Fatalf("ignored %+v", res.Ignored)
	}
}

// An index past the end of desired's associative list selects nothing:
// positions count in Git, and a live-only element has no Git position.
func TestPositionalIgnoreLiveOnlyElement(t *testing.T) {
	cfg := ignoreCfg(t, "/spec/template/spec/containers/2")
	cfg.ReportUnattributed = true // no history: report the live-only container
	e := New(Options{Config: cfg})
	l := u(t, twoContainers).Object
	setContainers(l, append(containers(l), map[string]any{"name": "extra", "image": "x:1"}))
	res := e.Diff(context.Background(), u(t, twoContainers), defaulted(t, l))
	if got := changeLines(res); len(got) != 1 || !strings.Contains(got[0], `[name="extra"]`) {
		t.Fatalf("changes %q", got)
	}
}

// Under the deduced schema (CRDs) every list is atomic and compared by
// position, so the rule prunes by position on each side, as before.
func TestPositionalIgnoreDeducedSchema(t *testing.T) {
	const cr = "apiVersion: example.com/v1\nkind: Widget\nmetadata: {name: w, namespace: ns}\nspec:\n  items: [{name: a, v: 1}, {name: b, v: 2}]\n"
	e := New(Options{Config: ignoreCfg(t, "/spec/items/1/v")})
	l := u(t, cr)
	l.Object["spec"].(map[string]any)["items"] = []any{map[string]any{"name": "a", "v": int64(1)}, map[string]any{"name": "b", "v": int64(9)}}
	res := e.Diff(context.Background(), u(t, cr), l)
	if res.Schema != schemaDeduced || res.Status != StatusInSync {
		t.Fatalf("schema %s changes %q", res.Schema, changeLines(res))
	}
}

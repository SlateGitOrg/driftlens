package scenarios

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/archit2501/driftlens/internal/cluster"
	"github.com/archit2501/driftlens/pkg/drift"
	"github.com/archit2501/driftlens/pkg/manifest"
	"github.com/archit2501/driftlens/pkg/rules"
)

// The mutation evaluation generates random drift with known ground truth:
// each trial applies a Deployment and a ConfigMap (server-side or
// client-side apply), then performs a random subset of Git edits and live
// edits whose correct classification is known by construction, and scores
// the engine's report against that truth.

type truth struct {
	kind, path string
	op         drift.Op
	origin     drift.Origin
	category   drift.Category
	manual     bool
	// limited marks ground truth the engine is documented not to recover
	// in this apply mode (see README "Limitations"); misses are counted in
	// the scores but do not fail the test.
	limited bool
}

type evalOp struct {
	name     string
	modes    string   // "ssa", "csa" or "both"
	limits   string   // apply modes where the engine cannot recover the truth
	excludes []string // operations touching the same field
	git      func(dep, cm *unstructured.Unstructured)
	live     *liveEdit
	want     func(mode string) []truth
}

type liveEdit struct {
	manager, sub, kind string
	pt                 types.PatchType
	patch              string
}

const imagePath = `.spec.template.spec.containers[name="app"].image`

func evalOps() []evalOp {
	setData := func(k, v string) func(_, cm *unstructured.Unstructured) {
		return func(_, cm *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(cm.Object, v, "data", k)
		}
	}
	one := func(t truth) func(string) []truth { return func(string) []truth { return []truth{t} } }
	return []evalOp{
		{name: "live-scale", modes: "both", excludes: []string{"git-replicas", "hpa-scale"},
			live: &liveEdit{"kubectl-scale", "scale", "Deployment", types.MergePatchType, `{"spec":{"replicas":7}}`},
			want: one(truth{"Deployment", ".spec.replicas", drift.OpModify, drift.OriginLive, drift.CategoryScale, true, false})},
		{name: "live-image", modes: "both", excludes: []string{"git-image"},
			live: &liveEdit{"kubectl-set", "", "Deployment", types.JSONPatchType, `[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"registry.example/app:9.9.9"}]`},
			want: one(truth{"Deployment", imagePath, drift.OpModify, drift.OriginLive, drift.CategoryImage, true, false})},
		{name: "live-label", modes: "both",
			live: &liveEdit{"kubectl-label", "", "Deployment", types.MergePatchType, `{"metadata":{"labels":{"debug":"true"}}}`},
			want: one(truth{"Deployment", ".metadata.labels.debug", drift.OpRemove, drift.OriginLive, drift.CategoryMetadata, true, false})},
		{name: "live-env-add", modes: "both", excludes: []string{"live-delete-env"},
			live: &liveEdit{"kubectl-edit", "", "Deployment", types.JSONPatchType, `[{"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"TRACE","value":"1"}}]`},
			want: one(truth{"Deployment", `.spec.template.spec.containers[name="app"].env[name="TRACE"]`, drift.OpRemove, drift.OriginLive, drift.CategorySpec, true, false})},
		{name: "controller-annotation", modes: "both",
			live: &liveEdit{"policy-controller", "", "Deployment", types.MergePatchType, `{"metadata":{"annotations":{"policy.example/checked":"yes"}}}`},
			want: one(truth{"Deployment", `.metadata.annotations["policy.example/checked"]`, drift.OpRemove, drift.OriginLive, drift.CategoryMetadata, false, false})},
		{name: "hpa-scale", modes: "both", excludes: []string{"live-scale", "git-replicas"},
			live: &liveEdit{"kube-controller-manager", "scale", "Deployment", types.MergePatchType, `{"spec":{"replicas":9}}`},
			want: func(string) []truth { return nil }}, // hidden by the eval's ignore rule
		{name: "git-image", modes: "both", excludes: []string{"live-image"},
			git: func(dep, _ *unstructured.Unstructured) {
				c, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
				c[0].(map[string]any)["image"] = "registry.example/app:1.1.0"
				_ = unstructured.SetNestedSlice(dep.Object, c, "spec", "template", "spec", "containers")
			},
			want: one(truth{"Deployment", imagePath, drift.OpModify, drift.OriginGit, drift.CategoryImage, false, false})},
		{name: "git-replicas", modes: "both", excludes: []string{"live-scale", "hpa-scale"},
			git: func(dep, _ *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(dep.Object, int64(4), "spec", "replicas")
			},
			want: one(truth{"Deployment", ".spec.replicas", drift.OpModify, drift.OriginGit, drift.CategoryScale, false, false})},
		{name: "git-config", modes: "both", excludes: []string{"conflict-config"},
			git:  setData("K1", "git-value"),
			want: one(truth{"ConfigMap", ".data.K1", drift.OpModify, drift.OriginGit, drift.CategorySpec, false, false})},
		{name: "live-config", modes: "both", excludes: []string{"conflict-config"},
			live: &liveEdit{"kubectl-edit", "", "ConfigMap", types.MergePatchType, `{"data":{"K2":"live-value"}}`},
			want: one(truth{"ConfigMap", ".data.K2", drift.OpModify, drift.OriginLive, drift.CategorySpec, true, false})},
		{name: "git-remove-key", modes: "both",
			git:  func(_, cm *unstructured.Unstructured) { unstructured.RemoveNestedField(cm.Object, "data", "K3") },
			want: one(truth{"ConfigMap", ".data.K3", drift.OpRemove, drift.OriginGit, drift.CategorySpec, false, false})},
		{name: "git-add-key", modes: "both", limits: "ssa",
			git:  setData("K4", "new"),
			want: one(truth{"ConfigMap", ".data.K4", drift.OpAdd, drift.OriginGit, drift.CategorySpec, false, false})},
		{name: "live-delete-env", modes: "both", limits: "both", excludes: []string{"live-env-add"},
			live: &liveEdit{"kubectl-patch", "", "Deployment", types.JSONPatchType, `[{"op":"remove","path":"/spec/template/spec/containers/0/env/0"}]`},
			// A deletion leaves no field owner: the actor (and so "manual")
			// is never recoverable, and without last-applied values neither
			// is the origin.
			want: one(truth{"Deployment", `.spec.template.spec.containers[name="app"].env[name="MODE"]`, drift.OpAdd, drift.OriginLive, drift.CategorySpec, true, false})},
		{name: "conflict-config", modes: "both", limits: "ssa", excludes: []string{"git-config", "live-config"},
			git:  setData("K1", "git-value"),
			live: &liveEdit{"kubectl-edit", "", "ConfigMap", types.MergePatchType, `{"data":{"K1":"live-value"}}`},
			want: one(truth{"ConfigMap", ".data.K1", drift.OpModify, drift.OriginConflict, drift.CategorySpec, true, false})},
	}
}

func evalObjects(ns string) (dep, cm *unstructured.Unstructured) {
	dep = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "app", "namespace": ns, "labels": map[string]any{"app": "app"}},
		"spec": map[string]any{
			"replicas": int64(2),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "app"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "app"}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "app", "image": "registry.example/app:1.0.0",
					"env": []any{map[string]any{"name": "MODE", "value": "prod"}},
				}}},
			},
		},
	}}
	cm = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg", "namespace": ns},
		"data":     map[string]any{"K1": "a", "K2": "b", "K3": "c"},
	}}
	return dep, cm
}

type score struct {
	limits                                         []string
	trials, expected, reported, matched            int
	originOK, categoryOK, manualOK, opOK, statusOK int
	statusTotal                                    int
	misses                                         []string
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

// TestMutationEval scores the engine on randomized, ground-truth-labelled
// drift. `make eval` runs it verbosely and writes eval/results.json.
func TestMutationEval(t *testing.T) {
	c := requireEnv(t)
	trials := 120
	if testing.Short() {
		trials = 20
	}
	cfg, err := rules.Parse([]byte("apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{name: hpa, managers: [kube-controller-manager]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	eng := drift.New(drift.Options{Config: cfg})
	seed := uint64(2026)
	if v := os.Getenv("DRIFTLENS_EVAL_SEED"); v != "" {
		if _, err := fmt.Sscan(v, &seed); err != nil {
			t.Fatalf("DRIFTLENS_EVAL_SEED: %v", err)
		}
	}
	rng := rand.New(rand.NewPCG(29, seed)) // #nosec G404 -- reproducible test data, not security
	ops := evalOps()
	scores := map[string]*score{"ssa": {}, "csa": {}}
	for trial := range trials {
		mode := []string{"ssa", "csa"}[trial%2]
		ns := fmt.Sprintf("eval-%03d", trial)
		chosen := pick(rng, ops, mode)
		dep, cm := evalObjects(ns)
		if err := setupTrial(t, c, mode, ns, dep, cm); err != nil {
			t.Fatal(err)
		}
		gitDep, gitCM := dep.DeepCopy(), cm.DeepCopy()
		var want []truth
		for _, op := range chosen {
			if op.git != nil {
				op.git(gitDep, gitCM)
			}
			if op.live != nil {
				target := dep
				if op.live.kind == "ConfigMap" {
					target = cm
				}
				if err := c.Patch(ctx(t), manifest.RefOf(target), op.live.pt, []byte(op.live.patch), op.live.manager, op.live.sub); err != nil {
					t.Fatalf("%s: %v", op.name, err)
				}
			}
			for _, w := range op.want(mode) {
				w.limited = op.limits == mode || op.limits == "both"
				want = append(want, w)
			}
		}
		rep, err := eng.Run(ctx(t), []*unstructured.Unstructured{gitDep, gitCM}, c)
		if err != nil {
			t.Fatal(err)
		}
		grade(scores[mode], rep, want, names(chosen))
	}
	results := map[string]any{"seed": seed, "trials": trials}
	for _, mode := range []string{"ssa", "csa"} {
		s := scores[mode]
		t.Logf("%s: %d trials, %d expected changes, %d reported; recall %s, precision %s; op %s, origin %s, category %s, manual %s; object status %s",
			mode, s.trials, s.expected, s.reported, pct(s.matched, s.expected), pct(s.matched, s.reported),
			pct(s.opOK, s.matched), pct(s.originOK, s.matched), pct(s.categoryOK, s.matched), pct(s.manualOK, s.matched), pct(s.statusOK, s.statusTotal))
		for _, m := range s.misses {
			t.Errorf("%s: %s", mode, m)
		}
		t.Logf("%s: %d documented-limitation misses (expected, see README)", mode, len(s.limits))
		results[mode] = map[string]any{
			"trials": s.trials, "expectedChanges": s.expected, "reportedChanges": s.reported, "matched": s.matched,
			"recall": pct(s.matched, s.expected), "precision": pct(s.matched, s.reported),
			"opAccuracy": pct(s.opOK, s.matched), "originAccuracy": pct(s.originOK, s.matched),
			"categoryAccuracy": pct(s.categoryOK, s.matched), "manualAccuracy": pct(s.manualOK, s.matched),
			"statusAccuracy": pct(s.statusOK, s.statusTotal), "documentedLimitationMisses": len(s.limits),
		}
	}
	if out := os.Getenv("DRIFTLENS_EVAL_OUT"); out != "" {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func setupTrial(t *testing.T, c *cluster.Client, mode, ns string, objs ...*unstructured.Unstructured) error {
	t.Helper()
	n := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
	if _, err := c.Apply(ctx(t), n, "eval-setup"); err != nil {
		return err
	}
	for _, o := range objs {
		var err error
		if mode == "csa" {
			_, err = c.CreateWithLastApplied(ctx(t), o, "kubectl-client-side-apply")
		} else {
			_, err = c.Apply(ctx(t), o, "driftlens")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pick chooses each applicable operation with probability 1/2, skipping
// operations that touch a field an already-chosen one touches.
func pick(rng *rand.Rand, ops []evalOp, mode string) []evalOp {
	var out []evalOp
	taken := map[string]bool{}
	for _, i := range rng.Perm(len(ops)) {
		op := ops[i]
		if (op.modes != "both" && op.modes != mode) || taken[op.name] || rng.IntN(2) == 0 {
			continue
		}
		out = append(out, op)
		taken[op.name] = true
		for _, x := range op.excludes {
			taken[x] = true
		}
	}
	return out
}

func names(ops []evalOp) string {
	n := make([]string, len(ops))
	for i, o := range ops {
		n[i] = o.name
	}
	sort.Strings(n)
	return strings.Join(n, ",")
}

func grade(s *score, rep *drift.Report, want []truth, label string) {
	s.trials++
	s.expected += len(want)
	got := map[string]drift.Change{}
	reported := 0
	for _, o := range rep.Objects {
		for _, ch := range o.Changes {
			got[o.Ref.Kind+" "+ch.Path] = ch
			reported++
		}
	}
	s.reported += reported
	expectStatus := map[string]drift.Status{"Deployment": drift.StatusInSync, "ConfigMap": drift.StatusInSync}
	limitedTrial := false
	for _, w := range want {
		limitedTrial = limitedTrial || w.limited
		st := drift.StatusDrifted
		if w.origin == drift.OriginGit {
			st = drift.StatusOutOfSync
		}
		if expectStatus[w.kind] != drift.StatusDrifted {
			expectStatus[w.kind] = st
		}
		miss := func(msg string) {
			if w.limited {
				s.limits = append(s.limits, msg)
			} else {
				s.misses = append(s.misses, msg)
			}
		}
		ch, ok := got[w.kind+" "+w.path]
		if !ok {
			miss(fmt.Sprintf("[%s] missed %s %s", label, w.kind, w.path))
			continue
		}
		s.matched++
		check := func(ok bool, n *int, what string, gotV, wantV any) {
			if ok {
				*n++
			} else {
				miss(fmt.Sprintf("[%s] %s %s: %s %v, want %v", label, w.kind, w.path, what, gotV, wantV))
			}
		}
		check(ch.Op == w.op, &s.opOK, "op", ch.Op, w.op)
		check(ch.Origin == w.origin, &s.originOK, "origin", ch.Origin, w.origin)
		check(ch.Category == w.category, &s.categoryOK, "category", ch.Category, w.category)
		check(ch.Manual == w.manual, &s.manualOK, "manual", ch.Manual, w.manual)
	}
	if reported != len(got) {
		s.misses = append(s.misses, fmt.Sprintf("[%s] duplicate change paths", label))
	}
	for _, o := range rep.Objects {
		s.statusTotal++
		switch msg := fmt.Sprintf("[%s] %s status %s, want %s", label, o.Ref.Kind, o.Status, expectStatus[o.Ref.Kind]); {
		case o.Status == expectStatus[o.Ref.Kind]:
			s.statusOK++
		case limitedTrial:
			s.limits = append(s.limits, msg)
		default:
			s.misses = append(s.misses, msg)
		}
	}
	// Precision: anything reported that is not expected is a false positive.
	for key := range got {
		found := false
		for _, w := range want {
			if w.kind+" "+w.path == key {
				found = true
			}
		}
		if !found {
			s.misses = append(s.misses, fmt.Sprintf("[%s] unexpected change %s", label, key))
		}
	}
}

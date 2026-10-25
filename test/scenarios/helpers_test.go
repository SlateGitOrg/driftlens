package scenarios

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SlateGitOrg/driftlens/internal/scenario"
	"github.com/SlateGitOrg/driftlens/pkg/drift"
	"github.com/SlateGitOrg/driftlens/pkg/normalize"
	"github.com/SlateGitOrg/driftlens/pkg/report"
)

var update = flag.Bool("update", false, "rewrite expected.txt / expected.json from the offline (snapshot) run")

const scenarioRoot = "../../testdata/scenarios"

// scenarios lists the scenario directories in a stable order.
func scenarios(t *testing.T) []*scenario.Scenario {
	t.Helper()
	entries, err := os.ReadDir(scenarioRoot)
	if err != nil {
		t.Fatal(err)
	}
	var out []*scenario.Scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := scenario.Load(filepath.Join(scenarioRoot, e.Name()))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		t.Fatal("no scenarios found")
	}
	return out
}

func engineFor(s *scenario.Scenario, d normalize.Defaulter) *drift.Engine {
	if d == nil {
		d = normalize.Static{}
		if s.Spec.Defaults == "none" {
			d = normalize.NoDefaults{}
		}
	}
	return drift.New(drift.Options{Config: s.Config, Defaulter: d})
}

func render(t *testing.T, rep *drift.Report) (txt, js []byte) {
	t.Helper()
	var a, b bytes.Buffer
	if err := report.Text(&a, rep, report.TextOptions{ShowIgnored: true, ShowInSync: true, Explain: true}); err != nil {
		t.Fatal(err)
	}
	if err := report.JSON(&b, rep); err != nil {
		t.Fatal(err)
	}
	return a.Bytes(), b.Bytes()
}

// compareGolden checks got against dir/name, rewriting it when allowed.
func compareGolden(t *testing.T, dir, name string, got []byte, write bool) {
	t.Helper()
	file := filepath.Join(dir, name)
	if write {
		if err := os.WriteFile(file, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatalf("%v (run `make golden` to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from golden file.\n--- want\n%s\n--- got\n%s", file, want, got)
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}

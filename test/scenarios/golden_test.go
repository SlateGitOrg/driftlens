package scenarios

import (
	"path/filepath"
	"testing"

	"github.com/archit2501/driftlens/pkg/drift"
	"github.com/archit2501/driftlens/pkg/manifest"
)

// TestGoldenOffline replays every scenario from its captured live snapshot
// (live.yaml, recorded from a real API server by the envtest suite) and
// compares the text and JSON reports with the golden files.
func TestGoldenOffline(t *testing.T) {
	for _, s := range scenarios(t) {
		t.Run(s.Name, func(t *testing.T) {
			live, err := manifest.Load([]string{filepath.Join(s.Dir, "live.yaml")}, nil)
			if err != nil {
				t.Fatalf("%v (run `make fixtures` to capture it from envtest)", err)
			}
			rep, err := engineFor(s, nil).Run(ctx(t), s.Desired, drift.NewSnapshotSource(live))
			if err != nil {
				t.Fatal(err)
			}
			txt, js := render(t, rep)
			compareGolden(t, s.Dir, "expected.txt", txt, *update)
			compareGolden(t, s.Dir, "expected.json", js, *update)
		})
	}
}

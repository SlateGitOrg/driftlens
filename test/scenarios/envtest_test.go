package scenarios

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SlateGitOrg/driftlens/internal/cluster"
	"github.com/SlateGitOrg/driftlens/internal/kubeenv"
	"github.com/SlateGitOrg/driftlens/internal/scenario"
	"github.com/SlateGitOrg/driftlens/pkg/manifest"
)

var updateLive = flag.Bool("update-live", false, "rewrite live.yaml snapshots captured from envtest")

var env *kubeenv.Env

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var err error
	env, err = kubeenv.Start()
	switch {
	case errors.Is(err, kubeenv.ErrNoAssets) && os.Getenv("DRIFTLENS_REQUIRE_ENVTEST") == "":
		fmt.Fprintln(os.Stderr, "envtest assets missing; envtest-backed tests will be skipped:", err)
		env = nil
	case err != nil:
		fmt.Fprintln(os.Stderr, "envtest:", err)
		return 1
	}
	code := m.Run()
	if env != nil {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stop envtest:", err)
		}
	}
	return code
}

func requireEnv(t *testing.T) *cluster.Client {
	t.Helper()
	if env == nil {
		t.Skip("envtest not available (set DRIFTLENS_REQUIRE_ENVTEST=1 to make this fatal)")
	}
	c, err := cluster.New(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestScenariosLive applies every scenario to a real API server, performs
// its live mutations, runs the engine against the cluster and asserts that
// the report equals the golden files — the same files the offline replay is
// checked against. With -update-live it also records live.yaml.
func TestScenariosLive(t *testing.T) {
	c := requireEnv(t)
	for _, s := range scenarios(t) {
		t.Run(s.Name, func(t *testing.T) {
			if err := s.Setup(ctx(t), c); err != nil {
				t.Fatal(err)
			}
			if *updateLive {
				snap, err := c.Snapshot(ctx(t), s.Desired, "default")
				if err != nil {
					t.Fatal(err)
				}
				scenario.Scrub(snap)
				b, err := manifest.EncodeList(snap)
				if err != nil {
					t.Fatal(err)
				}
				header := "# Captured from a real kube-apiserver (envtest) by `make fixtures`; do not edit.\n"
				if err := os.WriteFile(filepath.Join(s.Dir, "live.yaml"), append([]byte(header), b...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := engineFor(s, nil).Run(ctx(t), s.Desired, c)
			if err != nil {
				t.Fatal(err)
			}
			txt, js := render(t, rep)
			if *updateLive && *update {
				return // expected files are written by the offline replay
			}
			compareGolden(t, s.Dir, "expected.txt", txt, false)
			compareGolden(t, s.Dir, "expected.json", js, false)
		})
	}
}

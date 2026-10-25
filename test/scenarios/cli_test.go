package scenarios

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SlateGitOrg/driftlens/internal/cli"
	"github.com/SlateGitOrg/driftlens/pkg/drift"
	"github.com/SlateGitOrg/driftlens/pkg/manifest"
)

const cliManifests = `
apiVersion: v1
kind: Namespace
metadata: {name: cli-e2e}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: cli-e2e}
spec:
  replicas: 2
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers: [{name: api, image: "ghcr.io/example/api:1.0.0"}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {MODE: fast}
`

func runCLI(t *testing.T, sio cli.IO, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	sio.Out, sio.Err = &out, &errb
	if sio.In == nil {
		sio.In = strings.NewReader("")
	}
	code := cli.Run(ctx(t), args, sio)
	return code, out.String(), errb.String()
}

// TestCLIEndToEnd drives the real command line against envtest through a
// kubeconfig file: apply, verify in sync, scale by hand, detect the drift in
// text and JSON, snapshot, and replay the snapshot offline.
func TestCLIEndToEnd(t *testing.T) {
	c := requireEnv(t)
	dir := t.TempDir()
	kc, err := env.Kubeconfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	manifests := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(manifests, []byte(cliManifests), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := manifest.Decode(strings.NewReader(cliManifests), "cli")
	if err != nil {
		t.Fatal(err)
	}
	objs[2].SetNamespace("default")
	for _, o := range objs {
		if _, err := c.Apply(ctx(t), o, "argocd-controller"); err != nil {
			t.Fatal(err)
		}
	}
	base := []string{"diff", "--kubeconfig", kubeconfig, "-f", manifests}

	code, out, stderr := runCLI(t, cli.IO{}, base...)
	if code != cli.ExitOK {
		t.Fatalf("in-sync diff: exit %d\n%s%s", code, out, stderr)
	}
	if !strings.Contains(out, "3 objects, 3 in sync") {
		t.Fatalf("unexpected summary:\n%s", out)
	}

	ref := manifest.RefOf(objs[1])
	if err := c.Patch(ctx(t), ref, "application/merge-patch+json", []byte(`{"spec":{"replicas":6}}`), "kubectl-scale", "scale"); err != nil {
		t.Fatal(err)
	}

	code, out, _ = runCLI(t, cli.IO{}, append(base, "--color", "always")...)
	if code != cli.ExitDrift || !strings.Contains(out, "live drift by kubectl-scale (scale)") || !strings.Contains(out, "\x1b[") {
		t.Fatalf("drift diff: exit %d\n%s", code, out)
	}

	code, out, _ = runCLI(t, cli.IO{}, append(base, "-o", "json")...)
	if code != cli.ExitDrift {
		t.Fatalf("json diff: exit %d", code)
	}
	var rep drift.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Summary.ByStatus[drift.StatusDrifted] != 1 || rep.Summary.ByCategory[drift.CategoryScale] != 1 || rep.Summary.Manual != 1 {
		t.Fatalf("unexpected JSON summary: %+v", rep.Summary)
	}

	snap := filepath.Join(dir, "live.yaml")
	if code, _, stderr := runCLI(t, cli.IO{}, "snapshot", "--kubeconfig", kubeconfig, "-f", manifests, "--out", snap); code != cli.ExitOK {
		t.Fatalf("snapshot: exit %d: %s", code, stderr)
	}
	code, offline, _ := runCLI(t, cli.IO{}, "diff", "--live", snap, "-f", manifests, "-o", "json")
	if code != cli.ExitDrift || offline != out {
		t.Fatalf("offline replay differs from live diff:\n%s\nvs\n%s", offline, out)
	}

	// Server-side defaults through the CLI, read from stdin.
	code, out, stderr = runCLI(t, cli.IO{In: strings.NewReader(cliManifests)}, "diff", "--kubeconfig", kubeconfig, "-f", "-", "--defaults", "server", "--exit-code", "never")
	if code != cli.ExitOK || !strings.Contains(out, "1 drifted") {
		t.Fatalf("server defaults: exit %d\n%s%s", code, out, stderr)
	}

	// The REST config can also be injected directly (library use).
	code, _, _ = runCLI(t, cli.IO{RESTConfig: env.Config}, "diff", "-f", manifests, "--exit-code", "drift")
	if code != cli.ExitDrift {
		t.Fatalf("injected config: exit %d", code)
	}
}

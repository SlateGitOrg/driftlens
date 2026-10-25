package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/driftlens/pkg/drift"
)

const scen = "../../testdata/scenarios/"

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, IO{In: strings.NewReader(stdin), Out: &out, Err: &errb})
	return code, out.String(), errb.String()
}

func TestTopLevel(t *testing.T) {
	if code, _, errs := run(t, ""); code != ExitError || !strings.Contains(errs, "Usage:") {
		t.Fatal("no args")
	}
	if code, out, _ := run(t, "", "version"); code != ExitOK || !strings.HasPrefix(out, "driftlens ") {
		t.Fatal("version")
	}
	if code, out, _ := run(t, "", "help"); code != ExitOK || !strings.Contains(out, "snapshot") {
		t.Fatal("help")
	}
	if code, _, errs := run(t, "", "bogus"); code != ExitError || !strings.Contains(errs, `unknown command "bogus"`) {
		t.Fatal("unknown command")
	}
	if code, _, _ := run(t, "", "diff", "-h"); code != ExitOK {
		t.Fatal("-h should exit 0")
	}
}

func TestDiffOffline(t *testing.T) {
	d := scen + "manual-edits/"
	code, out, errs := run(t, "", "diff", "-f", d+"applied.yaml", "--live", d+"live.yaml", "--explain", "--show-ignored")
	if code != ExitDrift || !strings.Contains(out, "kubectl-scale (scale)") || !strings.Contains(out, "why:") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	code, out, _ = run(t, "", "diff", "-f", d+"applied.yaml", "--live", d+"live.yaml", "-o", "json", "--exit-code", "never")
	var rep drift.Report
	if code != ExitOK || json.Unmarshal([]byte(out), &rep) != nil || rep.Summary.Manual != 4 {
		t.Fatalf("json: exit %d %s", code, out)
	}
	g := scen + "ssa-git-change/"
	if code, _, _ := run(t, "", "diff", "-f", g+"desired.yaml", "--live", g+"live.yaml", "--exit-code", "drift"); code != ExitOK {
		t.Fatal("OutOfSync only must not fail --exit-code=drift")
	}
	if code, _, _ := run(t, "", "diff", "-f", g+"desired.yaml", "--live", g+"live.yaml"); code != ExitDrift {
		t.Fatal("OutOfSync fails --exit-code=any")
	}
	i := scen + "ignore-rules/"
	code, out, _ = run(t, "", "diff", "-f", i+"applied.yaml", "--live", i+"live.yaml", "--config", i+"config.yaml", "--show-ignored", "--color", "never")
	if code != ExitDrift || !strings.Contains(out, "ignored by hpa-owns-replicas") {
		t.Fatalf("config: %s", out)
	}
	k := scen + "kustomize-overlay/"
	code, out, _ = run(t, "", "diff", "-k", k+"overlays/prod", "--live", k+"live.yaml", "--defaults", "none", "--show-in-sync")
	if code != ExitDrift || !strings.Contains(out, "frontend:7.2.0") {
		t.Fatalf("kustomize: %s", out)
	}
	// Desired from stdin, as in `kustomize build | driftlens diff -f -`.
	in, err := os.ReadFile(scen + "missing-object/desired.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, string(in), "diff", "-f", "-", "--live", scen+"missing-object/live.yaml")
	if code != ExitDrift || !strings.Contains(out, "MISSING") {
		t.Fatalf("stdin: %s", out)
	}
}

func TestDiffUsageErrors(t *testing.T) {
	d := scen + "manual-edits/"
	cases := map[string][]string{
		"no desired":      {"diff", "--live", d + "live.yaml"},
		"bad output":      {"diff", "-f", d + "applied.yaml", "--live", d + "live.yaml", "-o", "yaml"},
		"bad exit code":   {"diff", "-f", d + "applied.yaml", "--live", d + "live.yaml", "--exit-code", "sometimes"},
		"bad defaults":    {"diff", "-f", d + "applied.yaml", "--live", d + "live.yaml", "--defaults", "magic"},
		"server offline":  {"diff", "-f", d + "applied.yaml", "--live", d + "live.yaml", "--defaults", "server"},
		"extra args":      {"diff", "-f", d + "applied.yaml", "stray"},
		"unknown flag":    {"diff", "--nope"},
		"missing config":  {"diff", "-f", d + "applied.yaml", "--live", d + "live.yaml", "--config", "/nonexistent"},
		"missing live":    {"diff", "-f", d + "applied.yaml", "--live", "/nonexistent.yaml"},
		"missing desired": {"diff", "-f", "/nonexistent.yaml", "--live", d + "live.yaml"},
		"bad kustomize":   {"diff", "-k", d, "--live", d + "live.yaml"},
		"bad kubeconfig":  {"diff", "-f", d + "applied.yaml", "--kubeconfig", "/nonexistent/kubeconfig"},
	}
	for name, args := range cases {
		if code, _, _ := run(t, "", args...); code != ExitError {
			t.Errorf("%s: exit %d, want %d", name, code, ExitError)
		}
	}
}

func TestSnapshotAndRulesErrors(t *testing.T) {
	if code, _, _ := run(t, "", "snapshot"); code != ExitError {
		t.Fatal("snapshot without desired")
	}
	if code, _, _ := run(t, "", "snapshot", "--bogus"); code != ExitError {
		t.Fatal("snapshot bad flag")
	}
	if code, _, _ := run(t, "", "snapshot", "-f", scen+"manual-edits/applied.yaml", "--kubeconfig", "/nonexistent"); code != ExitError {
		t.Fatal("snapshot bad kubeconfig")
	}
	if code, _, _ := run(t, "", "rules", "--config", "/nonexistent"); code != ExitError {
		t.Fatal("rules missing config")
	}
	if code, _, _ := run(t, "", "rules", "--bogus"); code != ExitError {
		t.Fatal("rules bad flag")
	}
}

func TestRules(t *testing.T) {
	code, out, _ := run(t, "", "rules")
	if code != ExitOK || !strings.Contains(out, "kubectl-client-side-apply") {
		t.Fatalf("default rules: %s", out)
	}
	code, out, _ = run(t, "", "rules", "--config", scen+"ignore-rules/config.yaml")
	if code != ExitOK || !strings.Contains(out, "# vpa-app-resources: .spec.template.spec.containers[] | select(.name == \"app\") | .resources (jq)") ||
		!strings.Contains(out, "(json-pointer)") {
		t.Fatalf("rules: %s", out)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{paths: [nope]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "", "rules", "--config", bad); code != ExitError || !strings.Contains(errs, "must start with") {
		t.Fatalf("invalid rules: %s", errs)
	}
}

func TestMulti(t *testing.T) {
	var m multi
	_ = m.Set("a")
	_ = m.Set("b")
	if m.String() != "a,b" {
		t.Fatal(m.String())
	}
	if usageError("x").Error() != "x" {
		t.Fatal("usageError")
	}
}

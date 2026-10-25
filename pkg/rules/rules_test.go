package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/driftlens/pkg/manifest"
)

func TestDefaultConfig(t *testing.T) {
	c := Default()
	if !c.IsGitOps("argocd-controller") || !c.IsGitOps("kubectl-client-side-apply") {
		t.Fatal("gitops managers")
	}
	if c.IsManual("kubectl-client-side-apply") {
		t.Fatal("GitOps takes precedence over manual globs")
	}
	for _, m := range []string{"kubectl", "kubectl-edit", "kubectl-scale", "k9s"} {
		if !c.IsManual(m) {
			t.Errorf("%s should be manual", m)
		}
	}
	if c.IsManual("kube-controller-manager") {
		t.Fatal("controller is not manual")
	}
}

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`
apiVersion: driftlens/v1
kind: DriftConfig
gitopsManagers: [flux*]
ignore:
  - match: {group: apps, kind: Deployment, namespace: "prod-*"}
    paths: [/spec/replicas, '.spec.template.spec.containers[] | select(.name == "app") | .resources']
  - name: hpa
    managers: [kube-controller-manager]
reportUnattributed: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsGitOps("flux-system") || c.IsGitOps("driftlens") || !c.IsManual("kubectl-edit") || !c.ReportUnattributed {
		t.Fatal("manager lists")
	}
	if c.Ignore[0].Name != "ignore[0]" || len(c.Ignore[0].Patterns()) != 2 {
		t.Fatalf("rule 0: %+v", c.Ignore[0])
	}
	prod := manifest.Ref{Group: "apps", Kind: "Deployment", Namespace: "prod-eu", Name: "x"}
	dev := manifest.Ref{Group: "apps", Kind: "Deployment", Namespace: "dev", Name: "x"}
	if len(c.RulesFor(prod)) != 2 || len(c.RulesFor(dev)) != 1 {
		t.Fatalf("RulesFor: %d / %d", len(c.RulesFor(prod)), len(c.RulesFor(dev)))
	}
	empty, err := Parse([]byte("  \n"))
	if err != nil || len(empty.GitOpsManagers) == 0 {
		t.Fatal("empty config should give defaults")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"kind":         "apiVersion: driftlens/v1\nkind: Other\n",
		"unknown":      "apiVersion: driftlens/v1\nkind: DriftConfig\nbogus: 1\n",
		"empty rule":   "apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{name: x}]\n",
		"bad path":     "apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{paths: [spec]}]\n",
		"bad glob":     "apiVersion: driftlens/v1\nkind: DriftConfig\nmanualManagers: ['[']\n",
		"empty glob":   "apiVersion: driftlens/v1\nkind: DriftConfig\ngitopsManagers: ['']\n",
		"bad match":    "apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{match: {kind: '['}, paths: [/a]}]\n",
		"bad rule mgr": "apiVersion: driftlens/v1\nkind: DriftConfig\nignore: [{managers: ['[']}]\n",
		"yaml":         "apiVersion: [\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(f, []byte("apiVersion: driftlens/v1\nkind: DriftConfig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(f); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file")
	}
}

func TestMatch(t *testing.T) {
	r := manifest.Ref{Group: "", Kind: "ConfigMap", Namespace: "a", Name: "web-1"}
	for m, want := range map[Match]bool{
		{}:                  true,
		{Kind: "*"}:         true,
		{Kind: "ConfigMap"}: true,
		{Kind: "Secret"}:    false,
		{Name: "web-*"}:     true,
		{Group: "apps"}:     false,
		{Namespace: "["}:    false,
	} {
		if got := m.Matches(r); got != want {
			t.Errorf("%+v.Matches = %v", m, got)
		}
	}
	if !strings.Contains(ConfigAPIVersion, "driftlens") {
		t.Fatal("api version")
	}
}

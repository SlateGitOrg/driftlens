package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const cm = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: n}\n"

func TestLoad(t *testing.T) {
	s, err := Load("../../testdata/scenarios/csa-three-way")
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.Manager != "kubectl-client-side-apply" || len(s.Applied) != 2 || len(s.Desired) != 2 || s.Config == nil {
		t.Fatalf("%+v", s.Spec)
	}
	k, err := Load("../../testdata/scenarios/kustomize-overlay")
	if err != nil || k.Spec.Manager != "driftlens" || k.Desired[0].GetNamespace() != "kustomize-overlay" {
		t.Fatalf("kustomize scenario: %v", err)
	}

	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("missing scenario.yaml")
	}
	write(t, dir, "scenario.yaml", "description: x\napply: magic\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("bad apply mode")
	}
	write(t, dir, "scenario.yaml", "description: x\nunknownField: 1\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("unknown field")
	}
	write(t, dir, "scenario.yaml", "description: x\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("missing applied.yaml")
	}
	write(t, dir, "applied.yaml", cm)
	s, err = Load(dir)
	if err != nil || len(s.Desired) != 1 {
		t.Fatalf("desired defaults to applied: %v", err)
	}
	write(t, dir, "config.yaml", "apiVersion: nope\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("bad config")
	}
	write(t, dir, "config.yaml", "")
	write(t, dir, "crds.yaml", "kind: [\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("bad crds")
	}
}

func TestTargetRef(t *testing.T) {
	r, err := Target{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "n", Name: "d"}.Ref()
	if err != nil || r.Group != "apps" || r.Version != "v1" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := (Target{Kind: "Deployment", Name: "d"}).Ref(); err == nil {
		t.Fatal("missing apiVersion")
	}
}

func TestScrub(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x", "uid": "abc", "managedFields": []any{map[string]any{"time": "now"}}},
	}}
	Scrub([]*unstructured.Unstructured{u, {Object: map[string]any{}}})
	if u.GetUID() != "00000000-0000-0000-0000-000000000000" || u.GetResourceVersion() != "1" {
		t.Fatal("uid/rv")
	}
	if u.Object["metadata"].(map[string]any)["managedFields"].([]any)[0].(map[string]any)["time"] != "2026-01-01T00:00:00Z" {
		t.Fatal("time")
	}
}

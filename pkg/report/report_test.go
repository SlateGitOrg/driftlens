package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SlateGitOrg/driftlens/pkg/drift"
	"github.com/SlateGitOrg/driftlens/pkg/manifest"
)

func sampleReport() *drift.Report {
	r := &drift.Report{APIVersion: drift.ReportAPIVersion, Defaults: "static", Objects: []drift.ObjectResult{
		{Ref: manifest.Ref{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "ns", Name: "web"}, Status: drift.StatusDrifted,
			Classes: []string{"image", "manual-edit"}, Basis: drift.BasisManagedFields, Schema: "openapi",
			Warnings: []string{"careful"},
			Changes: []drift.Change{
				{Path: ".spec.template.spec.containers[name=\"web\"].image", Op: drift.OpModify, Category: drift.CategoryImage, Origin: drift.OriginLive,
					Manual: true, Actors: []string{"kubectl-set"}, Desired: "a:1", Live: "a:2", LastApplied: "a:1", Note: "because"},
				{Path: ".spec.x", Op: drift.OpAdd, Category: drift.CategorySpec, Origin: drift.OriginUnknown, Desired: map[string]any{"k": []any{"v"}}},
				{Path: ".spec.y", Op: drift.OpRemove, Category: drift.CategorySpec, Origin: drift.OriginConflict, Live: int64(3)},
				{Path: ".spec.z", Op: drift.OpModify, Category: drift.CategorySpec, Origin: drift.OriginGit, Desired: false, Live: true},
			},
			Ignored: []drift.Ignored{{Path: ".spec.replicas", Rule: "hpa", Reason: "owned by kcm"}, {Path: ".spec.q", Rule: "-", Reason: "default"}}},
		{Ref: manifest.Ref{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "ok"}, Status: drift.StatusInSync},
		{Ref: manifest.Ref{Version: "v1", Kind: "Service", Namespace: "ns", Name: "gone"}, Status: drift.StatusMissing},
		{Ref: manifest.Ref{Version: "v1", Kind: "Secret", Namespace: "ns", Name: "bad"}, Status: drift.StatusError, Error: "forbidden"},
		{Ref: manifest.Ref{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "pending"}, Status: drift.StatusOutOfSync},
	}}
	r.Summarize()
	return r
}

func TestText(t *testing.T) {
	var b bytes.Buffer
	if err := Text(&b, sampleReport(), TextOptions{ShowIgnored: true, Explain: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"apps/v1 Deployment ns/web  DRIFTED  image, manual-edit  (basis: managed-fields, schema: openapi)",
		"warning: careful",
		"~ .spec.template.spec.containers[name=\"web\"].image  [image] live drift by kubectl-set (manual edit)",
		"why: because",
		"last-applied: a:1",
		"+ .spec.x  [spec] origin unknown",
		"        k:\n",
		"- .spec.y  [spec] conflict: Git and live both changed",
		"[spec] pending Git change",
		"= .spec.replicas  ignored by hpa: owned by kcm",
		"= .spec.q  ignored: default",
		"v1 Service ns/gone  MISSING",
		"error: forbidden",
		"OUT OF SYNC",
		"Summary: 5 objects, 1 in sync, 1 out of sync, 1 drifted, 1 missing, 1 error; 4 changes (1 image, 3 spec; 1 conflict, 1 git, 1 live, 1 unknown; 1 manual); 2 ignored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ns/ok") {
		t.Error("in-sync object printed without ShowInSync")
	}
	b.Reset()
	if err := Text(&b, sampleReport(), TextOptions{Color: true, ShowInSync: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\x1b[32min sync") || strings.Contains(b.String(), "why:") {
		t.Errorf("color/in-sync output wrong:\n%s", b.String())
	}
}

func TestEmptyReport(t *testing.T) {
	r := &drift.Report{}
	r.Summarize()
	var b bytes.Buffer
	if err := Text(&b, r, TextOptions{}); err != nil {
		t.Fatal(err)
	}
	if b.String() != "Summary: 0 objects; 0 changes\n" {
		t.Fatalf("%q", b.String())
	}
}

func TestJSON(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	var back drift.Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	web := back.Objects[len(back.Objects)-1]
	if back.APIVersion != drift.ReportAPIVersion || back.Summary.Changes != 4 || web.Ref.Name != "web" || web.Changes[3].Desired != false {
		t.Fatalf("round trip: %+v", back.Summary)
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("disk full")
	}
	f.n--
	return len(p), nil
}

func TestWriteErrors(t *testing.T) {
	if err := Text(&failWriter{n: 2}, sampleReport(), TextOptions{}); err == nil {
		t.Fatal("expected write error")
	}
	if err := JSON(&failWriter{}, sampleReport()); err == nil {
		t.Fatal("expected write error")
	}
}

func TestRenderValue(t *testing.T) {
	if got := renderValue("on"); got[0] != `"on"` {
		t.Fatalf("ambiguous string must be quoted: %v", got)
	}
	if got := renderValue(func() {}); len(got) != 1 {
		t.Fatalf("unmarshalable value: %v", got)
	}
}

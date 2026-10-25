package ownership

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

const managed = `[
 {"manager":"driftlens","operation":"Apply","apiVersion":"apps/v1","time":"2026-01-01T00:00:00Z",
  "fieldsV1":{"f:metadata":{"f:labels":{"f:app":{}}},
              "f:spec":{"f:replicas":{},
                        "f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{".":{},"f:image":{},"f:name":{}}}}},
                        "f:args":{}}}},
 {"manager":"kubectl-scale","operation":"Update","subresource":"scale","fieldsV1":{"f:spec":{"f:replicas":{}}}},
 {"manager":"kubectl-label","operation":"Update","fieldsV1":{"f:metadata":{"f:labels":{"f:debug":{}}}}},
 {"manager":"empty","operation":"Update"}
]`

func parse(t *testing.T) *Index {
	t.Helper()
	var raw []any
	if err := json.Unmarshal([]byte(managed), &raw); err != nil {
		t.Fatal(err)
	}
	ix, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func labels(es []Entry) string { return strings.Join(Managers(es), ",") }

func TestOwners(t *testing.T) {
	ix := parse(t)
	if ix.Empty() || len(ix.Entries) != 4 {
		t.Fatalf("entries: %d", len(ix.Entries))
	}
	if ix.Entries[0].Time.IsZero() || ix.Entries[0].Operation != OperationApply {
		t.Fatal("time/operation not decoded")
	}
	cases := []struct {
		path fieldpath.Path
		want string
	}{
		{fieldpath.MakePathOrDie("spec", "replicas"), "driftlens,kubectl-scale (scale)"},
		{fieldpath.MakePathOrDie("spec", "template", "spec", "containers", fieldpath.KeyByFields("name", "web"), "image"), "driftlens"},
		{fieldpath.MakePathOrDie("spec", "template", "spec", "containers", fieldpath.KeyByFields("name", "web")), "driftlens"},
		{fieldpath.MakePathOrDie("metadata", "labels"), "driftlens,kubectl-label"},
		{fieldpath.MakePathOrDie("metadata", "labels", "debug"), "kubectl-label"},
		// An element inside the atomic args list belongs to the list's owner.
		{append(fieldpath.MakePathOrDie("spec", "args"), fieldpath.PathElement{Index: intp(0)}), "driftlens"},
		{fieldpath.MakePathOrDie("spec", "paused"), ""},
	}
	for _, c := range cases {
		if got := labels(ix.Owners(c.path)); got != c.want {
			t.Errorf("Owners(%s) = %q, want %q", c.path, got, c.want)
		}
	}
	owned := ix.FieldsOwnedBy(func(m string) bool { return strings.HasPrefix(m, "kubectl") })
	if !owned.Has(fieldpath.MakePathOrDie("spec", "replicas")) || owned.Has(fieldpath.MakePathOrDie("spec", "args")) {
		t.Fatalf("FieldsOwnedBy: %v", owned)
	}
	var nilIx *Index
	if !nilIx.Empty() || nilIx.Owners(fieldpath.MakePathOrDie("a")) != nil || !nilIx.FieldsOwnedBy(func(string) bool { return true }).Empty() {
		t.Fatal("nil index must be empty")
	}
}

func intp(i int) *int { return &i }

func TestParseErrors(t *testing.T) {
	if _, err := Parse([]any{"x"}); err == nil {
		t.Fatal("non-object entry")
	}
	if _, err := Parse([]any{map[string]any{"manager": "m", "fieldsV1": map[string]any{"bogus": map[string]any{}}}}); err == nil {
		t.Fatal("bad fieldsV1 should fail")
	}
	if _, err := Parse([]any{map[string]any{"manager": "m", "fieldsV1": map[string]any{"f:x": func() {}}}}); err == nil {
		t.Fatal("unmarshalable fieldsV1 should fail")
	}
	ix, err := Parse(nil)
	if err != nil || !ix.Empty() {
		t.Fatal("empty input")
	}
}

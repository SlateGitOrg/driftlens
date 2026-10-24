// Package ownership decodes metadata.managedFields (server-side apply field
// ownership) and answers "who owns this field?".
package ownership

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// Operation is the managedFields operation type.
type Operation string

const (
	// OperationApply marks fields set by a server-side apply.
	OperationApply Operation = "Apply"
	// OperationUpdate marks fields set by create/update/patch requests.
	OperationUpdate Operation = "Update"
)

// Entry is one decoded managedFields entry.
type Entry struct {
	Manager     string
	Operation   Operation
	Subresource string
	APIVersion  string
	Time        time.Time
	Fields      *fieldpath.Set
}

// Label renders the manager with its subresource, e.g. "kubectl-scale (scale)".
func (e Entry) Label() string {
	if e.Subresource != "" {
		return e.Manager + " (" + e.Subresource + ")"
	}
	return e.Manager
}

// Index holds the decoded managedFields of one live object.
type Index struct {
	Entries []Entry
}

// Parse decodes raw managedFields (as found in an unstructured object).
func Parse(raw []any) (*Index, error) {
	ix := &Index{}
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("managedFields[%d]: not an object", i)
		}
		e := Entry{}
		e.Manager, _ = m["manager"].(string)
		op, _ := m["operation"].(string)
		e.Operation = Operation(op)
		e.Subresource, _ = m["subresource"].(string)
		e.APIVersion, _ = m["apiVersion"].(string)
		if ts, ok := m["time"].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				e.Time = t
			}
		}
		e.Fields = &fieldpath.Set{}
		if fv1, ok := m["fieldsV1"]; ok && fv1 != nil {
			b, err := json.Marshal(fv1)
			if err != nil {
				return nil, fmt.Errorf("managedFields[%d] (%s): %w", i, e.Manager, err)
			}
			if err := e.Fields.FromJSON(bytes.NewReader(b)); err != nil {
				return nil, fmt.Errorf("managedFields[%d] (%s): decode fieldsV1: %w", i, e.Manager, err)
			}
		}
		ix.Entries = append(ix.Entries, e)
	}
	return ix, nil
}

// Empty reports whether no ownership information is available.
func (ix *Index) Empty() bool { return ix == nil || len(ix.Entries) == 0 }

// Owners returns the entries that own p itself or any field below it. For
// an atomic value owned as a whole, the entry owning p is returned; for a
// granular map or list, everyone owning a member is returned.
func (ix *Index) Owners(p fieldpath.Path) []Entry {
	if ix == nil {
		return nil
	}
	var out []Entry
	for _, e := range ix.Entries {
		if e.Fields.Has(p) || !under(e.Fields, p).Empty() || ownsAncestorLeaf(e.Fields, p) {
			out = append(out, e)
		}
	}
	return out
}

// ownsAncestorLeaf reports whether s owns an ancestor of p as a leaf (an
// atomic value that contains p).
func ownsAncestorLeaf(s *fieldpath.Set, p fieldpath.Path) bool {
	for i := len(p) - 1; i > 0; i-- {
		anc := p[:i]
		if s.Has(anc) && under(s, anc).Empty() {
			// Has(anc) with no children: anc is a leaf, i.e. owned atomically.
			// Maps record themselves with "." and also have children, so they
			// are excluded by the WithPrefix check.
			return true
		}
	}
	return false
}

// under returns the subset of s below p (paths relative to p).
func under(s *fieldpath.Set, p fieldpath.Path) *fieldpath.Set {
	cur := s
	for _, pe := range p {
		child, ok := cur.Children.Get(pe)
		if !ok {
			return fieldpath.NewSet()
		}
		cur = child
	}
	return cur
}

// FieldsOwnedBy returns the union of field sets of entries whose manager
// satisfies pred.
func (ix *Index) FieldsOwnedBy(pred func(manager string) bool) *fieldpath.Set {
	out := &fieldpath.Set{}
	if ix == nil {
		return out
	}
	for _, e := range ix.Entries {
		if pred(e.Manager) {
			out = out.Union(e.Fields)
		}
	}
	return out
}

// Managers returns the sorted, de-duplicated labels of entries.
func Managers(entries []Entry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		l := e.Label()
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

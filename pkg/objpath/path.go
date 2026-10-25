// Package objpath models locations inside Kubernetes objects.
//
// A Path is a concrete location (".spec.template.spec.containers[name="web"].image").
// A Pattern is a user-supplied selector written either as an RFC 6901 JSON pointer
// ("/spec/replicas", with "*" as a wildcard segment) or as a jq-like expression
// (".spec.template.spec.containers[] | select(.name == "web") | .image").
// Patterns are used by ignore rules: they can prune matching values out of an
// object tree, and they can decide whether a concrete Path is covered by a rule.
package objpath

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// SegmentKind tells which part of a Segment is meaningful.
type SegmentKind int

const (
	// SegField selects a map member by name.
	SegField SegmentKind = iota
	// SegKey selects an element of an associative list by its key fields.
	SegKey
	// SegValue selects an element of a set-like list of scalars by value.
	SegValue
	// SegIndex selects an element of an atomic list by position.
	SegIndex
)

// KeyField is one key/value pair identifying an associative-list element.
type KeyField struct {
	Name  string
	Value any
}

// Segment is one step of a Path.
type Segment struct {
	Kind  SegmentKind
	Field string     // SegField
	Key   []KeyField // SegKey, sorted by Name
	Value any        // SegValue
	// Index is the element position. For SegIndex it is the selector itself;
	// for SegKey/SegValue it is the resolved position in the object the path
	// was resolved against, or -1 when unknown.
	Index int
}

// Path is a concrete location inside an object.
type Path []Segment

// Field returns a field segment.
func Field(name string) Segment { return Segment{Kind: SegField, Field: name, Index: -1} }

// Index returns an index segment.
func Index(i int) Segment { return Segment{Kind: SegIndex, Index: i} }

// Key returns an associative-list key segment with unresolved index.
func Key(fields ...KeyField) Segment {
	k := append([]KeyField(nil), fields...)
	sort.Slice(k, func(i, j int) bool { return k[i].Name < k[j].Name })
	return Segment{Kind: SegKey, Key: k, Index: -1}
}

// Fields builds a path made only of field segments.
func Fields(names ...string) Path {
	p := make(Path, len(names))
	for i, n := range names {
		p[i] = Field(n)
	}
	return p
}

var plainField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-]*$`)

// String renders the path in the jq-like notation used throughout driftlens
// output. Keyed elements render as [name="web"], set elements as [="x"],
// indexed elements as [3]. The empty path renders as ".".
func (p Path) String() string {
	if len(p) == 0 {
		return "."
	}
	var b strings.Builder
	for _, s := range p {
		switch s.Kind {
		case SegField:
			if plainField.MatchString(s.Field) {
				b.WriteByte('.')
				b.WriteString(s.Field)
			} else {
				b.WriteString("[")
				b.WriteString(quote(s.Field))
				b.WriteString("]")
			}
		case SegKey:
			b.WriteByte('[')
			for i, kf := range s.Key {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(kf.Name)
				b.WriteByte('=')
				b.WriteString(literal(kf.Value))
			}
			b.WriteByte(']')
		case SegValue:
			b.WriteString("[=")
			b.WriteString(literal(s.Value))
			b.WriteByte(']')
		case SegIndex:
			fmt.Fprintf(&b, "[%d]", s.Index)
		}
	}
	return b.String()
}

// Pointer renders the path as an RFC 6901 JSON pointer. It fails when a list
// element's position is unknown.
func (p Path) Pointer() (string, error) {
	if len(p) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, s := range p {
		b.WriteByte('/')
		switch s.Kind {
		case SegField:
			b.WriteString(EscapePointerToken(s.Field))
		default:
			if s.Index < 0 {
				return "", fmt.Errorf("list position of %s is unknown", p.String())
			}
			b.WriteString(strconv.Itoa(s.Index))
		}
	}
	return b.String(), nil
}

// HasPrefix reports whether q is a prefix of p (segment-wise, ignoring
// resolved indices of keyed elements).
func (p Path) HasPrefix(q Path) bool {
	if len(q) > len(p) {
		return false
	}
	for i := range q {
		if !p[i].sameSelector(q[i]) {
			return false
		}
	}
	return true
}

// Category helpers -------------------------------------------------------

// Last returns the last field name in the path, or "" if the final segment is
// not a field.
func (p Path) Last() string {
	if len(p) == 0 || p[len(p)-1].Kind != SegField {
		return ""
	}
	return p[len(p)-1].Field
}

func (s Segment) sameSelector(o Segment) bool {
	if s.Kind != o.Kind {
		return false
	}
	switch s.Kind {
	case SegField:
		return s.Field == o.Field
	case SegIndex:
		return s.Index == o.Index
	case SegValue:
		return reflect.DeepEqual(s.Value, o.Value)
	default:
		if len(s.Key) != len(o.Key) {
			return false
		}
		for i := range s.Key {
			if s.Key[i].Name != o.Key[i].Name || !reflect.DeepEqual(s.Key[i].Value, o.Key[i].Value) {
				return false
			}
		}
		return true
	}
}

// EscapePointerToken escapes a JSON pointer reference token (RFC 6901 §3).
func EscapePointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func literal(v any) string {
	switch t := v.(type) {
	case string:
		return quote(t)
	case nil:
		return "null"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// FromFieldPath converts a structured-merge-diff path into a Path, resolving
// keyed and set elements to positions using obj (which may be nil, in which
// case positions stay unknown). Resolution stops being attempted once the
// walk leaves obj.
func FromFieldPath(fp fieldpath.Path, obj any) Path {
	out := make(Path, 0, len(fp))
	cur := obj
	for _, pe := range fp {
		switch {
		case pe.FieldName != nil:
			out = append(out, Field(*pe.FieldName))
			cur = child(cur, *pe.FieldName)
		case pe.Key != nil:
			kfs := make([]KeyField, 0, len(*pe.Key))
			for _, f := range *pe.Key {
				kfs = append(kfs, KeyField{Name: f.Name, Value: f.Value.Unstructured()})
			}
			seg := Key(kfs...)
			seg.Index, cur = findKeyed(cur, seg.Key)
			out = append(out, seg)
		case pe.Value != nil:
			v := (*pe.Value).Unstructured()
			seg := Segment{Kind: SegValue, Value: v, Index: -1}
			seg.Index, cur = findValue(cur, v)
			out = append(out, seg)
		case pe.Index != nil:
			out = append(out, Index(*pe.Index))
			cur = elem(cur, *pe.Index)
		}
	}
	return out
}

// Get returns the value at p inside obj.
func Get(obj any, p Path) (any, bool) {
	cur := obj
	for _, s := range p {
		switch s.Kind {
		case SegField:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			v, ok := m[s.Field]
			if !ok {
				return nil, false
			}
			cur = v
		case SegIndex:
			l, ok := cur.([]any)
			if !ok || s.Index < 0 || s.Index >= len(l) {
				return nil, false
			}
			cur = l[s.Index]
		case SegKey:
			i, v := findKeyed(cur, s.Key)
			if i < 0 {
				return nil, false
			}
			cur = v
		case SegValue:
			i, v := findValue(cur, s.Value)
			if i < 0 {
				return nil, false
			}
			cur = v
		}
	}
	return cur, true
}

func child(cur any, name string) any {
	if m, ok := cur.(map[string]any); ok {
		return m[name]
	}
	return nil
}

func elem(cur any, i int) any {
	if l, ok := cur.([]any); ok && i >= 0 && i < len(l) {
		return l[i]
	}
	return nil
}

func findKeyed(cur any, key []KeyField) (int, any) {
	l, ok := cur.([]any)
	if !ok {
		return -1, nil
	}
	for i, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		match := true
		for _, kf := range key {
			if !ScalarEqual(m[kf.Name], kf.Value) {
				match = false
				break
			}
		}
		if match {
			return i, e
		}
	}
	return -1, nil
}

func findValue(cur any, v any) (int, any) {
	l, ok := cur.([]any)
	if !ok {
		return -1, nil
	}
	for i, e := range l {
		if ScalarEqual(e, v) {
			return i, e
		}
	}
	return -1, nil
}

// ScalarEqual compares two scalars, treating all numeric kinds by value.
func ScalarEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		fb, ok := toFloat(b)
		return ok && fa == fb
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Delete removes the value at p from obj. Keyed and set elements are found
// by their key or value, indexed elements by position; a removed list
// element is cut out of its list. It reports whether a value was removed.
func Delete(obj any, p Path) bool {
	if len(p) == 0 {
		return false
	}
	_, ok := del(obj, p)
	return ok
}

// del removes p below node and returns the value that must replace node
// (lists shrink, so their parent has to store the new slice header).
func del(node any, p Path) (any, bool) {
	s := p[0]
	if s.Kind == SegField {
		m, ok := node.(map[string]any)
		if !ok {
			return node, false
		}
		v, ok := m[s.Field]
		if !ok {
			return node, false
		}
		if len(p) == 1 {
			delete(m, s.Field)
			return m, true
		}
		nv, ok := del(v, p[1:])
		if ok {
			m[s.Field] = nv
		}
		return m, ok
	}
	l, ok := node.([]any)
	if !ok {
		return node, false
	}
	i := position(l, s)
	if i < 0 {
		return node, false
	}
	if len(p) == 1 {
		return slices.Delete(l, i, i+1), true
	}
	nv, ok := del(l[i], p[1:])
	if ok {
		l[i] = nv
	}
	return l, ok
}

// set stores v at p inside obj, replacing an existing value. It reports
// whether the location was found.
func set(obj any, p Path, v any) bool {
	parent, ok := Get(obj, p[:len(p)-1])
	if !ok {
		return false
	}
	s := p[len(p)-1]
	if s.Kind == SegField {
		m, ok := parent.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m[s.Field]; !ok {
			return false
		}
		m[s.Field] = v
		return true
	}
	l, ok := parent.([]any)
	if !ok {
		return false
	}
	i := position(l, s)
	if i < 0 {
		return false
	}
	l[i] = v
	return true
}

// position returns the index of the element of l that the list segment s
// names, or -1.
func position(l []any, s Segment) int {
	switch s.Kind {
	case SegIndex:
		if s.Index >= 0 && s.Index < len(l) {
			return s.Index
		}
	case SegKey:
		i, _ := findKeyed(l, s.Key)
		return i
	case SegValue:
		i, _ := findValue(l, s.Value)
		return i
	}
	return -1
}

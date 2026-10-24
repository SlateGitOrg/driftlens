package objpath

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type stepKind int

const (
	stepField        stepKind = iota // .name
	stepIndex                        // [3]
	stepEach                         // [] — every list element or map value
	stepFieldOrIndex                 // JSON pointer token that is a number
	stepSelect                       // select(cond) — filter on the current node
)

type cond struct {
	path  []string
	value any
}

type step struct {
	kind  stepKind
	name  string
	index int
	conds []cond // stepSelect: all must hold
}

// Pattern is a compiled path selector. It is safe for concurrent use.
type Pattern struct {
	src   string
	steps []step
}

// String returns the source text of the pattern.
func (p *Pattern) String() string { return p.src }

// Syntax tells how a pattern was written.
func (p *Pattern) Syntax() string {
	if strings.HasPrefix(p.src, "/") {
		return "json-pointer"
	}
	return "jq"
}

// Compile parses a pattern. Sources starting with "/" are JSON pointers
// (RFC 6901, plus "*" as a wildcard token); sources starting with "." are
// jq-like expressions.
func Compile(src string) (*Pattern, error) {
	s := strings.TrimSpace(src)
	var (
		steps []step
		err   error
	)
	switch {
	case strings.HasPrefix(s, "/"):
		steps, err = parsePointer(s)
	case strings.HasPrefix(s, "."):
		steps, err = parseJQ(s)
	default:
		return nil, fmt.Errorf("pattern %q: must start with '/' (JSON pointer) or '.' (jq path)", src)
	}
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", src, err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("pattern %q: selects the whole object", src)
	}
	if steps[0].kind == stepSelect {
		return nil, fmt.Errorf("pattern %q: select() must follow a path step", src)
	}
	return &Pattern{src: s, steps: steps}, nil
}

// MustCompile is Compile that panics on error; for package-level built-ins.
func MustCompile(src string) *Pattern {
	p, err := Compile(src)
	if err != nil {
		panic(err)
	}
	return p
}

func parsePointer(s string) ([]step, error) {
	var steps []step
	for _, tok := range strings.Split(s[1:], "/") {
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(tok, "~0", ""), "~1", ""), "~") {
			return nil, fmt.Errorf("invalid escape in token %q", tok)
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch {
		case tok == "*":
			steps = append(steps, step{kind: stepEach})
		case tok == "":
			return nil, errors.New("empty reference token")
		default:
			if n, err := strconv.Atoi(tok); err == nil && n >= 0 && strconv.Itoa(n) == tok {
				steps = append(steps, step{kind: stepFieldOrIndex, name: tok, index: n})
			} else {
				steps = append(steps, step{kind: stepField, name: tok})
			}
		}
	}
	return steps, nil
}

// jq-like parsing --------------------------------------------------------

type jqParser struct {
	s   string
	pos int
}

func parseJQ(s string) ([]step, error) {
	p := &jqParser{s: s}
	var steps []step
	for {
		p.skipSpace()
		if p.eof() {
			return steps, nil
		}
		switch c := p.s[p.pos]; {
		case c == '.':
			p.pos++
			if p.eof() || p.peekIs(' ') || p.peekIs('|') {
				continue // bare "." is the identity
			}
			switch p.s[p.pos] {
			case '"':
				name, err := p.str()
				if err != nil {
					return nil, err
				}
				steps = append(steps, step{kind: stepField, name: name})
			case '[':
				st, err := p.bracket()
				if err != nil {
					return nil, err
				}
				steps = append(steps, st)
			default:
				name := p.ident()
				if name == "" {
					return nil, p.errorf("expected field name")
				}
				steps = append(steps, step{kind: stepField, name: name})
			}
		case c == '[':
			st, err := p.bracket()
			if err != nil {
				return nil, err
			}
			steps = append(steps, st)
		case c == '|':
			p.pos++
			p.skipSpace()
			if strings.HasPrefix(p.s[p.pos:], "select") {
				st, err := p.selectExpr()
				if err != nil {
					return nil, err
				}
				steps = append(steps, st)
			} else if p.eof() || !p.peekIs('.') {
				return nil, p.errorf("expected '.' or select() after '|'")
			}
		default:
			return nil, p.errorf("unexpected %q", string(c))
		}
	}
}

func (p *jqParser) eof() bool          { return p.pos >= len(p.s) }
func (p *jqParser) peekIs(c byte) bool { return !p.eof() && p.s[p.pos] == c }

func (p *jqParser) skipSpace() {
	for !p.eof() && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *jqParser) errorf(format string, args ...any) error {
	return fmt.Errorf("at offset %d: %s", p.pos, fmt.Sprintf(format, args...))
}

func (p *jqParser) ident() string {
	start := p.pos
	for !p.eof() {
		c := p.s[p.pos]
		if c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	return p.s[start:p.pos]
}

func (p *jqParser) str() (string, error) {
	if !p.peekIs('"') {
		return "", p.errorf("expected string")
	}
	start := p.pos
	p.pos++
	for !p.eof() {
		switch p.s[p.pos] {
		case '\\':
			p.pos += 2
		case '"':
			p.pos++
			out, err := strconv.Unquote(p.s[start:p.pos])
			if err != nil {
				return "", p.errorf("bad string literal: %v", err)
			}
			return out, nil
		default:
			p.pos++
		}
	}
	return "", p.errorf("unterminated string")
}

// bracket parses [], [N], ["name"] and [k=v,k2=v2].
func (p *jqParser) bracket() (step, error) {
	p.pos++ // '['
	p.skipSpace()
	if p.peekIs(']') {
		p.pos++
		return step{kind: stepEach}, nil
	}
	if p.peekIs('"') {
		name, err := p.str()
		if err != nil {
			return step{}, err
		}
		p.skipSpace()
		if !p.peekIs(']') {
			return step{}, p.errorf("expected ']'")
		}
		p.pos++
		return step{kind: stepField, name: name}, nil
	}
	start := p.pos
	for !p.eof() && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if p.pos > start && p.peekIs(']') {
		n, err := strconv.Atoi(p.s[start:p.pos])
		if err != nil {
			return step{}, p.errorf("bad index: %v", err)
		}
		p.pos++
		return step{kind: stepIndex, index: n}, nil
	}
	p.pos = start
	var conds []cond
	for {
		p.skipSpace()
		key := p.ident()
		if key == "" {
			return step{}, p.errorf("expected key name in selector")
		}
		p.skipSpace()
		if !p.peekIs('=') {
			return step{}, p.errorf("expected '=' in selector")
		}
		p.pos++
		if p.peekIs('=') {
			p.pos++
		}
		p.skipSpace()
		v, err := p.literal(true)
		if err != nil {
			return step{}, err
		}
		conds = append(conds, cond{path: []string{key}, value: v})
		p.skipSpace()
		if p.peekIs(',') {
			p.pos++
			continue
		}
		if !p.peekIs(']') {
			return step{}, p.errorf("expected ']' or ','")
		}
		p.pos++
		break
	}
	// [k=v] is shorthand for "[] | select(.k == v)": represent it as two
	// steps by returning a select with an embedded iteration marker.
	return step{kind: stepEach, conds: conds}, nil
}

// selectExpr parses select(.a.b == lit and .c == lit).
func (p *jqParser) selectExpr() (step, error) {
	p.pos += len("select")
	p.skipSpace()
	if !p.peekIs('(') {
		return step{}, p.errorf("expected '(' after select")
	}
	p.pos++
	var conds []cond
	for {
		p.skipSpace()
		var path []string
		for p.peekIs('.') {
			p.pos++
			var name string
			if p.peekIs('"') {
				s, err := p.str()
				if err != nil {
					return step{}, err
				}
				name = s
			} else {
				name = p.ident()
			}
			if name == "" {
				return step{}, p.errorf("expected field in select condition")
			}
			path = append(path, name)
		}
		if len(path) == 0 {
			return step{}, p.errorf("select condition must start with a field path")
		}
		p.skipSpace()
		if !strings.HasPrefix(p.s[p.pos:], "==") {
			return step{}, p.errorf("only '==' comparisons are supported in select")
		}
		p.pos += 2
		p.skipSpace()
		v, err := p.literal(false)
		if err != nil {
			return step{}, err
		}
		conds = append(conds, cond{path: path, value: v})
		p.skipSpace()
		if strings.HasPrefix(p.s[p.pos:], "and") {
			p.pos += 3
			continue
		}
		if !p.peekIs(')') {
			return step{}, p.errorf("expected ')' or 'and'")
		}
		p.pos++
		return step{kind: stepSelect, conds: conds}, nil
	}
}

// literal parses a JSON scalar. With bare=true an unquoted identifier is
// accepted as a string (for the [name=web] shorthand).
func (p *jqParser) literal(bare bool) (any, error) {
	if p.peekIs('"') {
		return p.str()
	}
	start := p.pos
	for !p.eof() && !strings.ContainsRune(" \t,])", rune(p.s[p.pos])) {
		p.pos++
	}
	tok := p.s[start:p.pos]
	switch tok {
	case "":
		return nil, p.errorf("expected literal")
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return f, nil
	}
	if bare {
		return tok, nil
	}
	return nil, fmt.Errorf("at offset %d: invalid literal %q (quote strings)", start, tok)
}

// Evaluation -------------------------------------------------------------

func (c cond) holds(node any) bool {
	cur := node
	for _, f := range c.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[f]
		if !ok {
			return c.value == nil
		}
	}
	return ScalarEqual(cur, c.value)
}

func (s step) filter(node any) bool {
	for _, c := range s.conds {
		if !c.holds(node) {
			return false
		}
	}
	return true
}

// leadingSelects splits rest into its leading select steps and the remainder.
func leadingSelects(rest []step) ([]step, []step) {
	i := 0
	for i < len(rest) && rest[i].kind == stepSelect {
		i++
	}
	return rest[:i], rest[i:]
}

func allHold(sel []step, node any) bool {
	for _, s := range sel {
		if !s.filter(node) {
			return false
		}
	}
	return true
}

// Prune deletes every value selected by the pattern from obj and returns how
// many values were removed. Lists are compacted in place.
func (p *Pattern) Prune(obj map[string]any) int {
	n, _ := prune(obj, p.steps)
	return n
}

// prune applies steps to node. It returns the number of removed values and,
// for list nodes, the possibly shortened list that must replace node.
func prune(node any, steps []step) (int, any) {
	st := steps[0]
	sel, rest := leadingSelects(steps[1:])
	visit := func(child any) (remove bool, n int, replacement any) {
		if !st.filter(child) || !allHold(sel, child) {
			return false, 0, child
		}
		if len(rest) == 0 {
			return true, 1, nil
		}
		n, repl := prune(child, rest)
		return false, n, repl
	}
	removed := 0
	switch t := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if st.kind == stepEach || ((st.kind == stepField || st.kind == stepFieldOrIndex) && k == st.name) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			rm, n, repl := visit(t[k])
			removed += n
			if rm {
				delete(t, k)
			} else {
				t[k] = repl
			}
		}
		return removed, t
	case []any:
		out := t[:0]
		for i, e := range t {
			match := st.kind == stepEach || ((st.kind == stepIndex || st.kind == stepFieldOrIndex) && i == st.index)
			if !match {
				out = append(out, e)
				continue
			}
			rm, n, repl := visit(e)
			removed += n
			if !rm {
				out = append(out, repl)
			}
		}
		for i := len(out); i < len(t); i++ {
			t[i] = nil
		}
		return removed, out
	}
	return 0, node
}

// Relates reports whether the concrete path is selected by the pattern, lies
// below a selected value, or lies above one (so a change reported at path may
// have been caused by values the pattern ignores). root is the object the
// path was resolved against; it is used to evaluate select() conditions.
func (p *Pattern) Relates(path Path, root any) bool {
	cur := root
	j := 0
	for i := 0; i < len(p.steps); i++ {
		st := p.steps[i]
		if st.kind == stepSelect {
			if !st.filter(cur) {
				return false
			}
			continue
		}
		if j == len(path) {
			return true // path is an ancestor of a possible match
		}
		seg := path[j]
		if !segmentMatches(st, seg) {
			return false
		}
		cur = descend(cur, seg)
		if len(st.conds) > 0 && !st.filter(cur) {
			return false
		}
		j++
	}
	return true
}

func segmentMatches(st step, seg Segment) bool {
	switch st.kind {
	case stepEach:
		return true
	case stepField:
		return seg.Kind == SegField && seg.Field == st.name
	case stepIndex:
		return seg.Kind != SegField && seg.Index == st.index
	case stepFieldOrIndex:
		if seg.Kind == SegField {
			return seg.Field == st.name
		}
		return seg.Index == st.index
	}
	return false
}

func descend(cur any, seg Segment) any {
	if cur == nil {
		return nil
	}
	v, ok := Get(cur, Path{seg})
	if !ok {
		return nil
	}
	return v
}

// Find returns the concrete paths (with list positions) of every value the
// pattern selects in obj, in deterministic order. It does not modify obj.
func (p *Pattern) Find(obj any) []Path {
	var out []Path
	find(obj, p.steps, nil, &out)
	return out
}

func find(node any, steps []step, prefix Path, out *[]Path) {
	st := steps[0]
	sel, rest := leadingSelects(steps[1:])
	visit := func(child any, seg Segment) {
		if !st.filter(child) || !allHold(sel, child) {
			return
		}
		p := append(append(Path(nil), prefix...), seg)
		if len(rest) == 0 {
			*out = append(*out, p)
			return
		}
		find(child, rest, p, out)
	}
	switch t := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if st.kind == stepEach || ((st.kind == stepField || st.kind == stepFieldOrIndex) && k == st.name) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(t[k], Field(k))
		}
	case []any:
		for i, e := range t {
			if st.kind == stepEach || ((st.kind == stepIndex || st.kind == stepFieldOrIndex) && i == st.index) {
				visit(e, Index(i))
			}
		}
	}
}

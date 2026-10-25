package drift

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/archit2501/driftlens/pkg/objpath"
	"github.com/archit2501/driftlens/pkg/rules"
)

// pruneIgnored returns copies of the three sides (desired, live,
// last-applied) with the values selected by the rules' path patterns
// removed. Rules apply in order, each to the result of the previous one.
//
// Most patterns are pruned from each side on its own. A pattern with a list
// index ([N], or a numeric JSON pointer token) is different: the comparison
// matches associative lists (containers, env, ports, ...) by merge key, not
// by position, so pruning "containers/1" on each side would remove two
// different containers when the live order differs from Git. Such indices
// are counted in the desired object and the selected elements are pruned
// from every side by merge key. Indices into atomic lists (args, command,
// and every list under the deduced schema) are compared by position, so
// they keep selecting by position on each side.
func (e *Engine) pruneIgnored(full [3]*unstructured.Unstructured, ruleset []*rules.IgnoreRule) [3]*unstructured.Unstructured {
	var eff [3]*unstructured.Unstructured
	for i, o := range full {
		if o != nil {
			eff[i] = o.DeepCopy()
		}
	}
	keyed, checked := false, false
	for _, r := range ruleset {
		for _, p := range r.Patterns() {
			if p.Positional() {
				if !checked {
					// Same schema choice as the comparison: deduced means
					// every list is atomic and positions are the identity.
					_, name, _, err := e.typer.convert(full[0], full[1], full[2])
					keyed, checked = err == nil && name == schemaOpenAPI, true
				}
				if keyed {
					e.prunePositional(p, eff)
					continue
				}
			}
			for _, o := range eff {
				if o != nil {
					p.Prune(o.Object)
				}
			}
		}
	}
	return eff
}

// prunePositional prunes a pattern with index steps from all sides, reading
// indices into associative lists against the desired side (eff[0]).
func (e *Engine) prunePositional(p *objpath.Pattern, eff [3]*unstructured.Unstructured) {
	var del [3][]objpath.Path
	for i, o := range eff {
		if o == nil {
			continue
		}
		for _, m := range e.resolvePositional(p, o) {
			switch {
			case !m.keyed:
				del[i] = append(del[i], m.path) // atomic lists: by position, per side
			case i == 0:
				for j := range eff {
					del[j] = append(del[j], m.path) // by merge key on every side
				}
			}
			// A keyed match found only by counting positions on live or
			// last-applied is dropped: positions count in the desired object.
		}
	}
	for i, o := range eff {
		if o == nil {
			continue
		}
		// Find returns matches in list order; deleting from the end keeps
		// earlier positions valid.
		for k := len(del[i]) - 1; k >= 0; k-- {
			objpath.Delete(o.Object, del[i][k])
		}
	}
}

type positionalMatch struct {
	path  objpath.Path // list elements of associative lists named by key
	keyed bool         // an index step selected an associative-list element
}

// resolvePositional finds p's matches in o and rewrites every element of an
// associative or set list in them from its position to its merge key (or
// value), using o's typed field set to tell which lists are keyed.
func (e *Engine) resolvePositional(p *objpath.Pattern, o *unstructured.Unstructured) []positionalMatch {
	found := p.Find(o.Object)
	if len(found) == 0 {
		return nil
	}
	elems := keyedElements(e, o)
	positional := p.PositionalSegments()
	out := make([]positionalMatch, 0, len(found))
	for _, f := range found {
		m := positionalMatch{path: append(objpath.Path(nil), f...)}
		for k, s := range f {
			if s.Kind != objpath.SegIndex {
				continue
			}
			ptr, err := f[:k+1].Pointer()
			if err != nil {
				continue
			}
			if seg, ok := elems[ptr]; ok {
				m.path[k] = seg
				if k < len(positional) && positional[k] {
					m.keyed = true
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// keyedElements maps the JSON pointer of every associative- or set-list
// element in o to the segment that names it by key or value. It returns nil
// when o does not type with the OpenAPI schema.
func keyedElements(e *Engine, o *unstructured.Unstructured) map[string]objpath.Segment {
	tvs, name, _, err := e.typer.convert(o)
	if err != nil || name != schemaOpenAPI {
		return nil
	}
	set, err := tvs[0].ToFieldSet()
	if err != nil {
		return nil
	}
	out := map[string]objpath.Segment{}
	set.Iterate(func(fp fieldpath.Path) {
		for k := 1; k <= len(fp); k++ {
			last := fp[k-1]
			if last.Key == nil && last.Value == nil {
				continue
			}
			op := objpath.FromFieldPath(fp[:k], o.Object)
			ptr, err := op.Pointer()
			if err != nil {
				continue
			}
			if _, seen := out[ptr]; !seen {
				out[ptr] = op[k-1]
			}
		}
	})
	return out
}

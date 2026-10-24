package drift

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/typed"

	"github.com/archit2501/driftlens/pkg/manifest"
	"github.com/archit2501/driftlens/pkg/normalize"
	"github.com/archit2501/driftlens/pkg/objpath"
	"github.com/archit2501/driftlens/pkg/ownership"
	"github.com/archit2501/driftlens/pkg/rules"
)

// LiveSource provides live objects.
type LiveSource interface {
	// Get returns the live object named by ref, or (nil, nil) if it does
	// not exist.
	Get(ctx context.Context, ref manifest.Ref) (*unstructured.Unstructured, error)
	// Namespaced reports whether objects of gvk are namespaced.
	Namespaced(ctx context.Context, gvk schema.GroupVersionKind) (bool, error)
}

// Options configure an Engine.
type Options struct {
	Config           *rules.Config
	Defaulter        normalize.Defaulter
	DefaultNamespace string
	Logger           *slog.Logger
}

// Engine compares desired and live objects. It is safe for concurrent use
// as long as its Defaulter is.
type Engine struct {
	cfg       *rules.Config
	defaulter normalize.Defaulter
	ns        string
	log       *slog.Logger
	typer     *typer
}

// New returns an engine. Nil options take defaults: the built-in
// configuration, the static defaulter, namespace "default", and a discarding
// logger.
func New(o Options) *Engine {
	e := &Engine{cfg: o.Config, defaulter: o.Defaulter, ns: o.DefaultNamespace, log: o.Logger, typer: newTyper()}
	if e.cfg == nil {
		e.cfg = rules.Default()
	}
	if e.defaulter == nil {
		e.defaulter = normalize.Static{}
	}
	if e.ns == "" {
		e.ns = "default"
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	return e
}

// Run compares every desired object with its live counterpart.
func (e *Engine) Run(ctx context.Context, desired []*unstructured.Unstructured, live LiveSource) (*Report, error) {
	rep := &Report{APIVersion: ReportAPIVersion, Defaults: e.defaulter.Name()}
	objs := make([]*unstructured.Unstructured, 0, len(desired))
	for _, d := range desired {
		d = d.DeepCopy()
		if d.GetNamespace() == "" {
			namespaced, err := live.Namespaced(ctx, d.GroupVersionKind())
			if err != nil {
				rep.Objects = append(rep.Objects, ObjectResult{Ref: manifest.RefOf(d), Status: StatusError, Error: err.Error()})
				continue
			}
			if namespaced {
				d.SetNamespace(e.ns)
			}
		}
		objs = append(objs, d)
	}
	objs, dups := manifest.Dedupe(objs)
	for _, r := range dups {
		rep.Objects = append(rep.Objects, ObjectResult{Ref: r, Status: StatusError, Error: "object is defined more than once in the desired state"})
	}
	for _, d := range objs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref := manifest.RefOf(d)
		l, err := live.Get(ctx, ref)
		if err != nil {
			rep.Objects = append(rep.Objects, ObjectResult{Ref: ref, Status: StatusError, Error: err.Error()})
			continue
		}
		res := e.Diff(ctx, d, l)
		e.log.Debug("compared", "object", ref.String(), "status", res.Status, "changes", len(res.Changes))
		rep.Objects = append(rep.Objects, res)
	}
	rep.Summarize()
	return rep, nil
}

// side holds one normalized object of the three-way comparison.
type side struct {
	obj *unstructured.Unstructured
	tv  *typed.TypedValue
}

func (s side) get(p fieldpath.Path) (any, bool) {
	if s.obj == nil {
		return nil, false
	}
	return getFieldPath(s.obj.Object, p)
}

// Diff compares one desired object with its live object (nil when absent).
func (e *Engine) Diff(ctx context.Context, desired, live *unstructured.Unstructured) ObjectResult {
	ref := manifest.RefOf(desired)
	res := ObjectResult{Ref: ref}
	if live == nil {
		res.Status = StatusMissing
		return res
	}
	if err := e.diff(ctx, desired, live, &res); err != nil {
		res.Status = StatusError
		res.Error = err.Error()
		res.Changes = nil
		res.Ignored = nil
		return res
	}
	finish(&res)
	return res
}

func (e *Engine) diff(ctx context.Context, desired, live *unstructured.Unstructured, res *ObjectResult) error {
	gvk := desired.GroupVersionKind()
	if lg := live.GroupVersionKind(); lg.GroupKind() != gvk.GroupKind() {
		return fmt.Errorf("live object is %s, desired is %s", lg.GroupKind(), gvk.GroupKind())
	} else if lg.Version != gvk.Version {
		return fmt.Errorf("live object was read as %s but desired is %s: fetch live in the desired version", lg.Version, gvk.Version)
	}
	orig := normalize.PrepareDesired(desired)
	d, err := e.prepareApplied(ctx, orig, orig.Object)
	if err != nil {
		return fmt.Errorf("default desired: %w", err)
	}
	lp, err := normalize.PrepareLive(live, orig.Object)
	if err != nil {
		return err
	}
	var a *unstructured.Unstructured
	if lp.LastApplied != nil {
		a, err = e.prepareApplied(ctx, normalize.PrepareDesired(lp.LastApplied), orig.Object)
		if err != nil {
			return fmt.Errorf("default last-applied: %w", err)
		}
	}
	ix, err := ownership.Parse(lp.Managed)
	if err != nil {
		res.Warnings = append(res.Warnings, "ignoring unreadable managedFields: "+err.Error())
		ix = nil
	}
	switch {
	case a != nil:
		res.Basis = BasisLastApplied
	case !ix.Empty():
		res.Basis = BasisManagedFields
	default:
		res.Basis = BasisNone
	}

	ruleset := e.cfg.RulesFor(res.Ref)
	full := [3]*unstructured.Unstructured{d, lp.Object, a}
	eff := full
	if len(ruleset) > 0 {
		for i, o := range full {
			if o != nil {
				eff[i] = o.DeepCopy()
				for _, r := range ruleset {
					for _, p := range r.Patterns() {
						p.Prune(eff[i].Object)
					}
				}
			}
		}
	}

	tvs, schemaName, warn, err := e.typer.convert(eff[0], eff[1], eff[2])
	if err != nil {
		return err
	}
	res.Schema = schemaName
	if warn != "" {
		res.Warnings = append(res.Warnings, warn)
	}
	ignoredByManager := ix.FieldsOwnedBy(func(m string) bool {
		for _, r := range ruleset {
			if matchGlobs(r.Managers, m) {
				return true
			}
		}
		return false
	})
	for i, tv := range tvs {
		if tv != nil && !ignoredByManager.Empty() {
			tvs[i] = tv.RemoveItems(ignoredByManager)
		}
	}
	D, L, A := side{eff[0], tvs[0]}, side{eff[1], tvs[1]}, side{eff[2], tvs[2]}
	cmp, err := D.tv.Compare(L.tv)
	if err != nil {
		return fmt.Errorf("compare: %w", err)
	}
	c := classifier{cfg: e.cfg, ix: ix, basis: res.Basis, d: D, l: L, a: A, gvk: gvk}
	for _, p := range orderedPaths(cmp.Modified, false) {
		c.add(res, OpModify, p)
	}
	for _, p := range orderedPaths(cmp.Removed, true) {
		c.add(res, OpAdd, p)
	}
	for _, p := range orderedPaths(cmp.Added, true) {
		c.add(res, OpRemove, p)
	}
	if len(ruleset) > 0 || !ignoredByManager.Empty() {
		if err := e.explainIgnored(res, full, ruleset, ix, cmp); err != nil {
			return err
		}
	}
	return nil
}

// prepareApplied defaults an applied-side object (desired or last-applied)
// and re-strips anything the defaulter added that the server assigns, using
// reference (the user's desired object) to decide what was asked for.
func (e *Engine) prepareApplied(ctx context.Context, obj *unstructured.Unstructured, reference map[string]any) (*unstructured.Unstructured, error) {
	out, err := e.defaulter.Default(ctx, obj)
	if err != nil {
		return nil, err
	}
	o := runtime.DeepCopyJSON(out.Object)
	normalize.StripServerFields(o)
	gvk := obj.GroupVersionKind()
	normalize.StripAssigned(gvk.Group, gvk.Kind, o, reference)
	normalize.Canonicalize(o)
	return &unstructured.Unstructured{Object: o}, nil
}

// explainIgnored lists differences that exist without ignore rules but not
// with them, attributing each to the rule that hid it.
func (e *Engine) explainIgnored(res *ObjectResult, full [3]*unstructured.Unstructured, ruleset []*rules.IgnoreRule, ix *ownership.Index, effective *typed.Comparison) error {
	tvs, _, _, err := e.typer.convert(full[0], full[1])
	if err != nil {
		return err
	}
	all, err := tvs[0].Compare(tvs[1])
	if err != nil {
		return fmt.Errorf("compare without ignore rules: %w", err)
	}
	seen := map[string]bool{}
	for _, set := range []*fieldpath.Set{all.Modified, all.Removed, all.Added} {
		for _, p := range orderedPaths(set, true) {
			if effective.Modified.Has(p) || effective.Removed.Has(p) || effective.Added.Has(p) {
				continue
			}
			op := toObjPath(p, full[0].Object, full[1].Object)
			key := op.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			res.Ignored = append(res.Ignored, attribute(p, op, full, ruleset, ix))
		}
	}
	return nil
}

func attribute(p fieldpath.Path, op objpath.Path, full [3]*unstructured.Unstructured, ruleset []*rules.IgnoreRule, ix *ownership.Index) Ignored {
	for _, r := range ruleset {
		for _, pat := range r.Patterns() {
			if pat.Relates(op, full[0].Object) || pat.Relates(op, full[1].Object) {
				return Ignored{Path: op.String(), Rule: r.Name, Reason: "path " + pat.String()}
			}
		}
		if len(r.Managers) > 0 {
			for _, owner := range ix.Owners(p) {
				if matchGlobs(r.Managers, owner.Manager) {
					return Ignored{Path: op.String(), Rule: r.Name, Reason: "owned by " + owner.Label()}
				}
			}
		}
	}
	return Ignored{Path: op.String(), Rule: "-", Reason: "hidden by an ignore rule on a related path"}
}

func matchGlobs(globs []string, s string) bool { return rules.MatchGlobs(globs, s) }

// orderedPaths lists set members in structural order. With topmost, members
// whose ancestor is also a member are dropped (an added subtree is reported
// once, at its root).
func orderedPaths(set *fieldpath.Set, topmost bool) []fieldpath.Path {
	var out []fieldpath.Path
	set.Iterate(func(p fieldpath.Path) {
		if topmost {
			for i := 1; i < len(p); i++ {
				if set.Has(p[:i]) {
					return
				}
			}
		}
		out = append(out, p.Copy())
	})
	return out
}

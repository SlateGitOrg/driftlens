package drift

import (
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/archit2501/driftlens/pkg/objpath"
	"github.com/archit2501/driftlens/pkg/ownership"
	"github.com/archit2501/driftlens/pkg/rules"
)

// classifier turns raw structural differences into explained Changes.
type classifier struct {
	cfg     *rules.Config
	ix      *ownership.Index
	basis   Basis
	d, l, a side
	gvk     schema.GroupVersionKind
}

// owners splits the live owners of p into GitOps and other managers.
type owners struct {
	gitops      []ownership.Entry
	gitopsApply bool
	other       []ownership.Entry
	manual      bool
}

func (c *classifier) owners(p fieldpath.Path) owners {
	var o owners
	for _, e := range c.ix.Owners(p) {
		if c.cfg.IsGitOps(e.Manager) {
			o.gitops = append(o.gitops, e)
			if e.Operation == ownership.OperationApply {
				o.gitopsApply = true
			}
			continue
		}
		o.other = append(o.other, e)
		if c.cfg.IsManual(e.Manager) {
			o.manual = true
		}
	}
	return o
}

func (c *classifier) add(res *ObjectResult, op Op, p fieldpath.Path) {
	dv, dok := c.d.get(p)
	lv, lok := c.l.get(p)
	av, aok := c.a.get(p)
	own := c.owners(p)

	ch := Change{Op: op}
	var objp objpath.Path
	if op == OpRemove {
		objp = objpath.FromFieldPath(p, c.l.obj.Object)
	} else {
		objp = objpath.FromFieldPath(p, c.d.obj.Object)
	}
	ch.Path = objp.String()
	if ptr, err := objp.Pointer(); err == nil {
		ch.Pointer = ptr
	}
	if dok {
		ch.Desired = dv
	}
	if lok {
		ch.Live = lv
	}
	if aok {
		ch.LastApplied = av
	}

	switch op {
	case OpModify:
		ch.Origin, ch.Note = c.originModify(dv, lv, av, aok, own)
	case OpAdd:
		ch.Origin, ch.Note = c.originAdd(aok)
	case OpRemove:
		var report bool
		ch.Origin, ch.Note, report = c.originRemove(aok, own)
		if !report {
			res.Ignored = append(res.Ignored, Ignored{Path: ch.Path, Rule: "-", Reason: ch.Note})
			return
		}
	}
	if ch.Origin == OriginLive || ch.Origin == OriginConflict {
		ch.Actors = ownership.Managers(own.other)
		ch.Manual = own.manual
	}
	ch.Category = category(c.gvk, objp, own)
	res.Changes = append(res.Changes, ch)
}

func (c *classifier) originModify(dv, lv, av any, aok bool, own owners) (Origin, string) {
	switch c.basis {
	case BasisLastApplied:
		switch {
		case aok && equal(dv, av):
			return OriginLive, "live differs from the last applied value; Git did not change"
		case aok && equal(lv, av):
			return OriginGit, "Git changed since the last apply; live still has the applied value"
		case aok:
			return OriginConflict, "Git and live both changed since the last apply"
		case len(own.other) > 0:
			return OriginConflict, "Git sets a field that another manager changed live"
		default:
			return OriginGit, "Git now sets a field that holds a server default"
		}
	case BasisManagedFields:
		switch {
		case len(own.gitops) > 0:
			return OriginGit, "the GitOps manager owns the live value, so Git changed"
		case len(own.other) > 0:
			return OriginLive, "another field manager took ownership of this field"
		default:
			return OriginGit, "Git now sets a field that holds a server default"
		}
	}
	return OriginUnknown, "no last-applied state or field ownership to compare against"
}

func (c *classifier) originAdd(aok bool) (Origin, string) {
	if c.basis == BasisLastApplied {
		if aok {
			return OriginLive, "the field was applied but has been removed live"
		}
		return OriginGit, "Git added the field since the last apply"
	}
	return OriginUnknown, "the field is absent live; without last-applied values a Git addition and a live deletion look the same"
}

// originRemove classifies a field present only live. The bool is false when
// the field should not be reported (an unattributed server default).
func (c *classifier) originRemove(aok bool, own owners) (Origin, string, bool) {
	switch c.basis {
	case BasisLastApplied:
		if aok {
			return OriginGit, "Git removed the field since the last apply; applying will prune it", true
		}
	case BasisManagedFields:
		if len(own.other) == 0 && own.gitopsApply {
			return OriginGit, "Git removed the field; the GitOps manager still owns it, so the next apply prunes it", true
		}
	}
	if len(own.other) > 0 {
		return OriginLive, "the field was added live by another field manager", true
	}
	if c.cfg.ReportUnattributed {
		return OriginUnknown, "the field exists only live and nothing records who set it", true
	}
	return "", "live-only field with no owner outside GitOps: treated as a server default", false
}

var scaleKinds = map[string]bool{
	"apps/Deployment": true, "apps/StatefulSet": true, "apps/ReplicaSet": true, "/ReplicationController": true,
}

var containerLists = map[string]bool{"containers": true, "initContainers": true, "ephemeralContainers": true}

// category decides what kind of field a path is.
func category(gvk schema.GroupVersionKind, p objpath.Path, own owners) Category {
	if len(p) > 0 && p[0].Kind == objpath.SegField && p[0].Field == "metadata" {
		return CategoryMetadata
	}
	if len(p) == 2 && p[0].Field == "spec" && p[1].Field == "replicas" && scaleKinds[gvk.Group+"/"+gvk.Kind] {
		return CategoryScale
	}
	for _, e := range append(own.other, own.gitops...) {
		if e.Subresource == "scale" {
			return CategoryScale
		}
	}
	if n := len(p); n >= 3 && p[n-1].Kind == objpath.SegField && p[n-1].Field == "image" &&
		p[n-2].Kind != objpath.SegField && p[n-3].Kind == objpath.SegField && containerLists[p[n-3].Field] {
		return CategoryImage
	}
	return CategorySpec
}

// finish sets the status and classes of a compared object.
func finish(res *ObjectResult) {
	classes := map[string]bool{}
	status := StatusInSync
	for _, ch := range res.Changes {
		classes[string(ch.Category)] = true
		if ch.Manual {
			classes[ClassManualEdit] = true
		}
		switch ch.Origin {
		case OriginGit:
			if status == StatusInSync {
				status = StatusOutOfSync
			}
		default:
			status = StatusDrifted
		}
	}
	res.Status = status
	res.Classes = res.Classes[:0]
	for k := range classes {
		res.Classes = append(res.Classes, k)
	}
	sort.Strings(res.Classes)
	if len(res.Classes) == 0 {
		res.Classes = nil
	}
}

func getFieldPath(obj map[string]any, p fieldpath.Path) (any, bool) {
	return objpath.Get(obj, objpath.FromFieldPath(p, nil))
}

// toObjPath converts p, resolving list positions in the first object that
// contains it.
func toObjPath(p fieldpath.Path, objs ...map[string]any) objpath.Path {
	for _, o := range objs {
		if _, ok := getFieldPath(o, p); ok {
			return objpath.FromFieldPath(p, o)
		}
	}
	return objpath.FromFieldPath(p, nil)
}

// equal compares two decoded JSON values, treating numbers by value.
func equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	if objpath.ScalarEqual(a, b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

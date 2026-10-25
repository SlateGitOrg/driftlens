// Package drift is the driftlens engine: it compares desired objects with
// live objects (and, when available, the last-applied configuration and the
// server-side-apply field ownership) and explains every difference.
package drift

import (
	"sort"

	"github.com/archit2501/driftlens/pkg/manifest"
)

// ReportAPIVersion versions the JSON report schema.
const ReportAPIVersion = "driftlens/v1"

// Status is the verdict for one object.
type Status string

const (
	// StatusInSync means desired and live agree after normalization.
	StatusInSync Status = "InSync"
	// StatusOutOfSync means only Git moved: applying desired would fix it.
	StatusOutOfSync Status = "OutOfSync"
	// StatusDrifted means the live object was changed outside Git (or the
	// origin of a difference cannot be determined).
	StatusDrifted Status = "Drifted"
	// StatusMissing means the object is in Git but not in the cluster.
	StatusMissing Status = "Missing"
	// StatusError means the object could not be compared.
	StatusError Status = "Error"
)

// Op describes a difference from the point of view of applying desired.
type Op string

const (
	// OpModify means both sides have the field with different values.
	OpModify Op = "modify"
	// OpAdd means desired has the field, live does not.
	OpAdd Op = "add"
	// OpRemove means live has the field, desired does not.
	OpRemove Op = "remove"
)

// Category is what kind of field changed.
type Category string

const (
	// CategoryImage is a container image change.
	CategoryImage Category = "image"
	// CategoryScale is a replica count change.
	CategoryScale Category = "scale"
	// CategoryMetadata is a label/annotation/other metadata change.
	CategoryMetadata Category = "metadata"
	// CategorySpec is any other desired-state field (spec, data, rules...).
	CategorySpec Category = "spec"
)

// ClassManualEdit marks an object with at least one live change attributed
// to a manual field manager.
const ClassManualEdit = "manual-edit"

// Origin tells which side moved, from the three-way comparison.
type Origin string

const (
	// OriginGit means desired changed since the last apply; live still holds the
	// applied value. Applying desired resolves it.
	OriginGit Origin = "git"
	// OriginLive means live changed since the last apply (out-of-band edit).
	OriginLive Origin = "live"
	// OriginConflict means both desired and live changed since the last apply.
	OriginConflict Origin = "conflict"
	// OriginUnknown means there is not enough history to tell.
	OriginUnknown Origin = "unknown"
)

// Basis names the evidence used for the three-way comparison.
type Basis string

const (
	// BasisLastApplied uses the kubectl last-applied-configuration annotation.
	BasisLastApplied Basis = "last-applied"
	// BasisManagedFields uses server-side-apply field ownership.
	BasisManagedFields Basis = "managed-fields"
	// BasisNone means only a two-way comparison was possible.
	BasisNone Basis = "none"
)

// Change is one explained difference.
type Change struct {
	Path        string   `json:"path"`
	Pointer     string   `json:"pointer,omitempty"`
	Op          Op       `json:"op"`
	Category    Category `json:"category"`
	Origin      Origin   `json:"origin"`
	Manual      bool     `json:"manual"`
	Actors      []string `json:"actors,omitempty"`
	Desired     any      `json:"desired,omitempty"`
	Live        any      `json:"live,omitempty"`
	LastApplied any      `json:"lastApplied,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// Ignored is a difference hidden by configuration or by attribution rules.
type Ignored struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

// ObjectResult is the verdict for one desired object.
type ObjectResult struct {
	Ref      manifest.Ref `json:"ref"`
	Status   Status       `json:"status"`
	Classes  []string     `json:"classes,omitempty"`
	Basis    Basis        `json:"basis,omitempty"`
	Schema   string       `json:"schema,omitempty"`
	Changes  []Change     `json:"changes,omitempty"`
	Ignored  []Ignored    `json:"ignored,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
	Error    string       `json:"error,omitempty"`
}

// Summary aggregates a report.
type Summary struct {
	Objects    int              `json:"objects"`
	ByStatus   map[Status]int   `json:"byStatus"`
	Changes    int              `json:"changes"`
	ByCategory map[Category]int `json:"byCategory"`
	ByOrigin   map[Origin]int   `json:"byOrigin"`
	Manual     int              `json:"manualChanges"`
	Ignored    int              `json:"ignored"`
}

// Report is the result of comparing a set of objects.
type Report struct {
	APIVersion string         `json:"apiVersion"`
	Defaults   string         `json:"defaults"`
	Summary    Summary        `json:"summary"`
	Objects    []ObjectResult `json:"objects"`
}

// HasDrift reports whether any object is not in sync.
func (r *Report) HasDrift() bool {
	for _, o := range r.Objects {
		if o.Status != StatusInSync {
			return true
		}
	}
	return false
}

// HasLiveDrift reports whether any object drifted in the cluster (as opposed
// to being merely out of sync with a newer Git revision), is missing, or failed.
func (r *Report) HasLiveDrift() bool {
	for _, o := range r.Objects {
		switch o.Status {
		case StatusDrifted, StatusMissing, StatusError:
			return true
		}
	}
	return false
}

// Summarize recomputes the summary from the objects and sorts them.
func (r *Report) Summarize() {
	sort.SliceStable(r.Objects, func(i, j int) bool { return r.Objects[i].Ref.Less(r.Objects[j].Ref) })
	s := Summary{
		ByStatus:   map[Status]int{},
		ByCategory: map[Category]int{},
		ByOrigin:   map[Origin]int{},
	}
	for _, o := range r.Objects {
		s.Objects++
		s.ByStatus[o.Status]++
		s.Ignored += len(o.Ignored)
		for _, c := range o.Changes {
			s.Changes++
			s.ByCategory[c.Category]++
			s.ByOrigin[c.Origin]++
			if c.Manual {
				s.Manual++
			}
		}
	}
	r.Summary = s
}

// Package report renders drift reports as human-readable text or JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/archit2501/driftlens/pkg/drift"
)

// TextOptions tune the text renderer.
type TextOptions struct {
	Color       bool // ANSI colors
	ShowIgnored bool // list differences hidden by ignore rules or attribution
	ShowInSync  bool // print a line for objects without differences
	Explain     bool // print the reasoning behind each origin
}

const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
)

type painter struct{ on bool }

func (p painter) c(code, s string) string {
	if !p.on {
		return s
	}
	return code + s + ansiReset
}

// JSON writes the report as indented JSON.
func JSON(w io.Writer, r *drift.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Text writes the report in the human-readable format.
func Text(w io.Writer, r *drift.Report, o TextOptions) error {
	tw := &errWriter{w: w}
	p := painter{on: o.Color}
	for _, obj := range r.Objects {
		if obj.Status == drift.StatusInSync && !o.ShowInSync && !(o.ShowIgnored && len(obj.Ignored) > 0) {
			continue
		}
		writeObject(tw, p, obj, o)
	}
	if len(r.Objects) > 0 {
		tw.printf("\n")
	}
	tw.printf("%s\n", SummaryLine(r))
	return tw.err
}

func writeObject(w *errWriter, p painter, obj drift.ObjectResult, o TextOptions) {
	header := p.c(ansiBold, obj.Ref.String()) + "  " + statusLabel(p, obj.Status)
	if len(obj.Classes) > 0 {
		header += "  " + strings.Join(obj.Classes, ", ")
	}
	if obj.Basis != "" && obj.Status != drift.StatusInSync {
		header += p.c(ansiDim, "  (basis: "+string(obj.Basis)+", schema: "+obj.Schema+")")
	}
	w.printf("%s\n", header)
	if obj.Error != "" {
		w.printf("    error: %s\n", obj.Error)
	}
	for _, warn := range obj.Warnings {
		w.printf("    %s\n", p.c(ansiYellow, "warning: "+warn))
	}
	for _, ch := range obj.Changes {
		writeChange(w, p, ch, o)
	}
	if o.ShowIgnored {
		for _, ig := range obj.Ignored {
			w.printf("  %s\n", p.c(ansiDim, fmt.Sprintf("= %s  ignored by %s: %s", ig.Path, ig.Rule, ig.Reason)))
		}
	}
}

func statusLabel(p painter, s drift.Status) string {
	switch s {
	case drift.StatusInSync:
		return p.c(ansiGreen, "in sync")
	case drift.StatusOutOfSync:
		return p.c(ansiYellow, "OUT OF SYNC")
	case drift.StatusDrifted:
		return p.c(ansiRed, "DRIFTED")
	case drift.StatusMissing:
		return p.c(ansiRed, "MISSING")
	default:
		return p.c(ansiRed, "ERROR")
	}
}

func writeChange(w *errWriter, p painter, ch drift.Change, o TextOptions) {
	var mark, color string
	switch ch.Op {
	case drift.OpAdd:
		mark, color = "+", ansiGreen
	case drift.OpRemove:
		mark, color = "-", ansiRed
	default:
		mark, color = "~", ansiYellow
	}
	w.printf("  %s %s  [%s] %s\n", p.c(color, mark), ch.Path, ch.Category, origin(ch))
	if o.Explain && ch.Note != "" {
		w.printf("      %s\n", p.c(ansiDim, "why: "+ch.Note))
	}
	if ch.Desired != nil {
		writeValue(w, p, "desired:", ch.Desired, ansiGreen)
	}
	if ch.Live != nil {
		writeValue(w, p, "live:", ch.Live, ansiRed)
	}
	if ch.LastApplied != nil && ch.Op == drift.OpModify {
		writeValue(w, p, "last-applied:", ch.LastApplied, ansiCyan)
	}
}

func origin(ch drift.Change) string {
	var s string
	switch ch.Origin {
	case drift.OriginGit:
		s = "pending Git change"
	case drift.OriginLive:
		s = "live drift"
	case drift.OriginConflict:
		s = "conflict: Git and live both changed"
	default:
		s = "origin unknown"
	}
	if len(ch.Actors) > 0 {
		s += " by " + strings.Join(ch.Actors, ", ")
	}
	if ch.Manual {
		s += " (manual edit)"
	}
	return s
}

func writeValue(w *errWriter, p painter, label string, v any, color string) {
	lines := renderValue(v)
	pad := fmt.Sprintf("      %-13s", label)
	if len(lines) == 1 {
		w.printf("%s%s\n", pad, p.c(color, lines[0]))
		return
	}
	w.printf("%s\n", strings.TrimRight(pad, " "))
	for _, l := range lines {
		w.printf("        %s\n", p.c(color, l))
	}
}

// renderValue renders a value as YAML lines (one line for scalars).
func renderValue(v any) []string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return []string{fmt.Sprint(v)}
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// SummaryLine renders the one-line summary of a report.
func SummaryLine(r *drift.Report) string {
	s := r.Summary
	parts := []string{fmt.Sprintf("%d objects", s.Objects)}
	for _, st := range []drift.Status{drift.StatusInSync, drift.StatusOutOfSync, drift.StatusDrifted, drift.StatusMissing, drift.StatusError} {
		if n := s.ByStatus[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(statusWord(st))))
		}
	}
	line := strings.Join(parts, ", ") + fmt.Sprintf("; %d changes", s.Changes)
	if s.Changes > 0 {
		line += " (" + counts(s.ByCategory) + "; " + counts(s.ByOrigin) + fmt.Sprintf("; %d manual)", s.Manual)
	}
	if s.Ignored > 0 {
		line += fmt.Sprintf("; %d ignored", s.Ignored)
	}
	return "Summary: " + line
}

func statusWord(s drift.Status) string {
	switch s {
	case drift.StatusInSync:
		return "in sync"
	case drift.StatusOutOfSync:
		return "out of sync"
	default:
		return string(s)
	}
}

func counts[K ~string](m map[K]int) string {
	keys := make([]string, 0, len(m))
	for k, n := range m {
		if n > 0 {
			keys = append(keys, string(k))
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", m[K(k)], k)
	}
	return strings.Join(parts, ", ")
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

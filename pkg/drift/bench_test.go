package drift

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/SlateGitOrg/driftlens/pkg/manifest"
)

// loadPair returns the manual-edits scenario: a Deployment with four kinds
// of drift and real managedFields captured from an API server.
func loadPair(b *testing.B) (desired, live *unstructured.Unstructured) {
	b.Helper()
	d, err := manifest.Load([]string{"../../testdata/scenarios/manual-edits/applied.yaml"}, nil)
	if err != nil {
		b.Fatal(err)
	}
	l, err := manifest.Load([]string{"../../testdata/scenarios/manual-edits/live.yaml"}, nil)
	if err != nil {
		b.Fatal(err)
	}
	return d[0], l[0]
}

// BenchmarkDiffDrifted measures one three-way comparison of a drifted
// Deployment (normalize, default, type, compare, attribute).
func BenchmarkDiffDrifted(b *testing.B) {
	d, l := loadPair(b)
	e := New(Options{})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if res := e.Diff(ctx, d, l); len(res.Changes) != 4 {
			b.Fatalf("unexpected result %+v", res)
		}
	}
}

// BenchmarkRun measures a full run over n copies of the drifted Deployment
// served from a snapshot.
func BenchmarkRun(b *testing.B) {
	d, l := loadPair(b)
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("objects=%d", n), func(b *testing.B) {
			desired := make([]*unstructured.Unstructured, n)
			live := make([]*unstructured.Unstructured, n)
			for i := range n {
				desired[i] = d.DeepCopy()
				desired[i].SetName(fmt.Sprintf("api-%04d", i))
				live[i] = l.DeepCopy()
				live[i].SetName(fmt.Sprintf("api-%04d", i))
			}
			src := NewSnapshotSource(live)
			e := New(Options{})
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				rep, err := e.Run(ctx, desired, src)
				if err != nil || rep.Summary.Changes != 4*n {
					b.Fatalf("run: %v %+v", err, rep.Summary)
				}
			}
		})
	}
}

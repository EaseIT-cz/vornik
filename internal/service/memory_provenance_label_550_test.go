package service

// GitHub #550 (found at: production probe, 2026-10-25: all 60 REST hits had an
// empty provenance label). Only the companion adapter labelled hits, so the
// architect's memory_search (the REST path) never saw "verified", and a
// prompt rule keyed on it would cite nothing. Design:
// https://docs.vornik.io §1.2 (corrected).

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/api"
)

type fakeLabeler struct {
	labels map[string]string
	err    error
	asked  []string
}

func (f *fakeLabeler) ChunkProvenanceLabels(_ context.Context, _ string, ids []string) (map[string]string, error) {
	f.asked = append(f.asked, ids...)
	return f.labels, f.err
}

func TestLabelHitsProvenance(t *testing.T) {
	hits := []api.MemorySearchResult{{ChunkID: "design"}, {ChunkID: "review"}}
	l := &fakeLabeler{labels: map[string]string{"design": "verified"}}
	labelHitsProvenance(context.Background(), l, "p", hits)
	if hits[0].Provenance != "verified" || hits[1].Provenance != "" || len(l.asked) != 2 {
		t.Fatalf("hits %+v, asked %v", hits, l.asked)
	}
	// A lookup failure never fails the search; the labels stay empty.
	hits = []api.MemorySearchResult{{ChunkID: "design"}}
	labelHitsProvenance(context.Background(), &fakeLabeler{err: errors.New("db down")}, "p", hits)
	if hits[0].Provenance != "" {
		t.Fatalf("hits %+v", hits)
	}
	labelHitsProvenance(context.Background(), nil, "p", hits) // nil labeler is a no-op
}

func TestMemorySearchAdapter_LabelsProvenance(t *testing.T) {
	a, ok := newMemorySearchAdapter(nil, &fakeLabeler{}).(*memorySearchAdapter)
	if !ok || a.labeler == nil {
		t.Fatal("the REST search adapter must carry the provenance labeler")
	}
}

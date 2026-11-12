//go:build integration

package memory_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// GitHub #701 (memory rollback x supersession design, A.8). Found at
// production: after 3 docs were deleted and 28 moved, 347 chunks across 32
// old paths were still served, and nothing listed or retired a path-identity
// document by path.
func TestIntegration_MissingDocuments_701(t *testing.T) {
	f := newDocFixture(t)
	repo := memory.NewRepository(f.db)
	ctx := context.Background()
	gone := f.artifact("gone", "INPUT", "upload", 0)
	gone2 := f.artifact("gone2", "INPUT", "upload", 1)
	kept := f.artifact("kept", "INPUT", "upload", 0)
	f.chunks(gone, "docs/old/a.md", docScope, "spec", "", 2)
	f.chunks(gone2, "docs/old/a.md", docScope, "spec", "", 3)
	f.chunks(kept, docPath, docScope, "spec", "", 1)
	f.chunks(f.artifact("bare", "INPUT", "upload", 0), "bare.md", docScope, "spec", "", 1)              // a bare name: not a path document
	f.chunks(f.artifact("agent", "OUTPUT", "task_output", 0), "docs/agent.md", docScope, "spec", "", 1) // agent output: never listed
	other := f.artifact("other", "INPUT", "upload", 0)
	f.chunks(other, "docs/old/a.md", "github.com/acme/other", "spec", "", 1) // other scope

	docs, err := repo.PathDocuments(ctx, f.project, docScope)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, d := range docs {
		got[d.Name] = d.Chunks
	}
	if len(got) != 2 || got["docs/old/a.md"] != 5 || got[docPath] != 1 {
		t.Fatalf("path documents = %+v, want docs/old/a.md (5) and %s (1)", docs, docPath)
	}
	n, err := repo.SupersedeMissingDocument(ctx, f.project, docScope, "docs/old/a.md")
	if err != nil || n != 5 {
		t.Fatalf("retired %d (%v), want every servable chunk of the path (5)", n, err)
	}
	sameIDs(t, f.live("docs/old/a.md"), other) // live() spans scopes: only the other scope's copy is left
	sameIDs(t, f.live(docPath), kept)
	if n, _ := repo.SupersedeMissingDocument(ctx, f.project, docScope, "bare.md"); n != 0 {
		t.Fatal("a bare name is never a path document")
	}
}

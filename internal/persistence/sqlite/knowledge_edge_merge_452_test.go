package sqlite

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #452 (memory-knowledge-graph design §2.1, 2026-11-08). Found at
// review (code audit). UpsertEdge read the existing row, merged in Go, and
// wrote with a separate UPDATE outside any transaction, so a second upsert of
// the same edge landing between the read and the write was overwritten: its
// source chunk was lost. The seam runs the second upsert at exactly that
// point.
func TestKnowledgeEdgeUpsert452_ConcurrentMergeLosesNoChunk(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := NewKnowledgeEdgeRepository(db.DB)
	edge := func(chunk string) *persistence.KnowledgeEdge {
		return &persistence.KnowledgeEdge{ProjectID: "proj-452", FromEntity: "ent-a", ToEntity: "ent-b", Predicate: "employs", SourceChunks: []string{chunk}}
	}
	if err := repo.UpsertEdge(ctx, edge("chunk-1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var once sync.Once
	var wg sync.WaitGroup
	var racerErr error
	edgeMergeReadHook = func() {
		once.Do(func() {
			wg.Add(1)
			done := make(chan struct{})
			go func() {
				defer wg.Done()
				defer close(done)
				racerErr = repo.UpsertEdge(ctx, edge("chunk-3"))
			}()
			// Old code: the racer reads and writes now, before this upsert
			// writes. Fixed code: it waits for this transaction.
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	t.Cleanup(func() { edgeMergeReadHook = nil })

	if err := repo.UpsertEdge(ctx, edge("chunk-2")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	wg.Wait()
	if racerErr != nil {
		t.Fatalf("racing upsert: %v", racerErr)
	}

	var raw string
	if err := db.QueryRowContext(ctx, `SELECT source_chunks FROM knowledge_edges WHERE project_id = 'proj-452'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got sqliteStringArray
	if err := got.Scan(raw); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "chunk-1,chunk-2,chunk-3" {
		t.Fatalf("source_chunks = %v, want all three chunks: a concurrent upsert's chunk was lost", []string(got))
	}
}

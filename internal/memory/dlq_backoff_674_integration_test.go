//go:build integration

package memory_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
)

// GitHub #674 (project RAG memory design, "the DLQ backoff survives a
// replay"): DLQReplay deleted the DLQ row, so the next failure inserted a
// fresh one at retry_count 0 and a 10-minute backoff, and a dead endpoint was
// retried every 10 minutes forever. The history now rides on the queue row.
func TestIntegration_DLQBackoff674_GrowsAcrossReplays(t *testing.T) {
	f := newDocFixture(t)
	art := f.artifact("dlq674", "INPUT", "upload", 0)
	f.chunks(art, "docs/dlq674.md", docScope, "spec", "", 1)
	chunkID := art + "-c0"
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM memory_embed_dlq WHERE chunk_id = $1`, chunkID)
		_, _ = f.db.Exec(`DELETE FROM memory_embed_queue WHERE chunk_id = $1`, chunkID)
	})
	ctx := context.Background()
	repo := memory.NewRepository(f.db)
	move := func() {
		t.Helper()
		if err := repo.DLQMove(ctx, chunkID, f.project, "embedding_failed", "endpoint down", time.Now().Add(10*time.Minute)); err != nil {
			t.Fatalf("DLQMove: %v", err)
		}
	}
	read := func() (int, time.Time, time.Time) {
		t.Helper()
		var count int
		var after, first time.Time
		if err := f.db.QueryRow(`SELECT retry_count, retry_after, first_failed_at FROM memory_embed_dlq WHERE chunk_id = $1`, chunkID).
			Scan(&count, &after, &first); err != nil {
			t.Fatal(err)
		}
		return count, after, first
	}
	replay := func() {
		t.Helper()
		if _, err := repo.DLQReplay(ctx, []string{chunkID}); err != nil {
			t.Fatalf("DLQReplay: %v", err)
		}
	}

	move()
	_, _, first := read()
	time.Sleep(10 * time.Millisecond)
	replay()
	move()
	replay()
	move()
	count, after, firstNow := read()
	if d := time.Until(after); count != 2 || d < 39*time.Minute || d > 41*time.Minute {
		t.Fatalf("after two replays: retry_count %d, retry_after in %v; want 2 and about 40 min", count, d)
	}
	if !firstNow.Equal(first) {
		t.Fatalf("first_failed_at moved from %v to %v across replays", first, firstNow)
	}
}

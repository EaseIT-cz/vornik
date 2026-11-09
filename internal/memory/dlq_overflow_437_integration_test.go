//go:build integration

package memory_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
)

// GitHub #437: the DLQ backoff cast power(2, retry_count + 1) to int, which
// overflows at retry_count >= 30, so the move failed (and the worker then lost
// the chunk). Capped, a 31st move succeeds with the 24-hour ceiling.
func TestIntegration_DLQMove437_NoOverflowAtThirtyRetries(t *testing.T) {
	f := newDocFixture(t)
	art := f.artifact("dlq", "INPUT", "upload", 0)
	f.chunks(art, "docs/dlq.md", docScope, "spec", "", 1)
	chunkID := art + "-c0"
	if _, err := f.db.Exec(`INSERT INTO memory_embed_dlq (chunk_id, project_id, reason, last_error, retry_count, retry_after, first_failed_at, last_failed_at)
		VALUES ($1, $2, 'embedding_failed', 'x', 30, now(), now(), now())`, chunkID, f.project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM memory_embed_dlq WHERE chunk_id = $1`, chunkID) })
	if err := memory.NewRepository(f.db).DLQMove(context.Background(), chunkID, f.project, "embedding_failed", "again", time.Now()); err != nil {
		t.Fatalf("DLQMove at retry_count 30: %v", err)
	}
	var count int
	var after time.Time
	if err := f.db.QueryRow(`SELECT retry_count, retry_after FROM memory_embed_dlq WHERE chunk_id = $1`, chunkID).Scan(&count, &after); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(after); count != 31 || d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("retry_count %d, retry_after in %v; want 31 and about 24 h", count, d)
	}
}

//go:build integration

package memory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #298 (2026-10-12): refuted and superseded facts must not reach
// either consolidator's shared input reader and reappear as fresh memory.
func TestIntegration_ConsolidationInputOnlyLiveChunks(t *testing.T) {
	db := openIngestRecallDB(t)
	project := persistence.GenerateID("itest-gh298")
	other := project + "-other"
	t.Cleanup(func() { cleanupProjects(t, db, project, other) })
	base := time.Now().Add(-time.Hour)
	// Legacy is eligible. Both terminal statuses are newer than every live
	// row; refuted is newest, so LIMIT 1 exercises filtering before LIMIT.
	for i, status := range []string{"verified", "unverified", "legacy", "superseded", "refuted"} {
		text := "consolidation-" + status
		_, err := db.Exec(`INSERT INTO project_memory_chunks
			(id, project_id, source_name, content, content_hash, validation_status, created_at)
			VALUES ($1,$2,'test.md',$3,$4,$5,$6)`, fmt.Sprintf("%s-%d", project, i), project,
			text, memory.ContentHash(text), status, base.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	// This newer live foreign-project row must never consume this project's
	// LIMIT slot or appear in its default-limit (1000) full result set.
	seedListRows(t, db, other, 1, time.Now(), 0)
	_, err := db.Exec(`INSERT INTO project_memory_chunks
		(id,project_id,source_name,content,content_hash,validation_status,created_at,expires_at)
		VALUES ($1,$2,'test.md','expired',$3,'verified',now(),now()-interval '1 minute')`,
		project+"-expired", project, memory.ContentHash("expired"))
	if err != nil {
		t.Fatal(err)
	}
	repo := memory.NewRepository(db)
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{1, "[consolidation-legacy]"},
		{0, "[consolidation-legacy consolidation-unverified consolidation-verified]"},
	} {
		got, err := repo.ListChunkContents(context.Background(), project, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != tc.want {
			t.Fatalf("limit %d: got %v, want %s", tc.limit, got, tc.want)
		}
	}
}

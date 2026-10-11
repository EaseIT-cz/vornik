//go:build integration

package memory_test

// GitHub #119 item 6 and #287 (2026-10-08): the user sees what is kept,
// paged, and a forgotten note leaves every listing (LLD 22, "Memory the user
// can see"). Behavioural, on the pgvector lane.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

func seedListRows(t *testing.T, db *sql.DB, project string, n int, base time.Time, sameInstantEvery int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%04d", project, i)
		at := base.Add(time.Duration(i) * time.Second)
		if sameInstantEvery > 0 {
			at = base.Add(time.Duration(i/sameInstantEvery) * time.Second) // ties on created_at
		}
		text := fmt.Sprintf("listing note %d of %s", i, project)
		if _, err := db.Exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, created_at)
			VALUES ($1, $2, 'companion:hermes:note', $3, $4, $5)`, id, project, text, memory.ContentHash(text), at); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func pageAll(t *testing.T, s *memory.Searcher, project string, limit int) ([]string, []int) {
	t.Helper()
	var ids []string
	var sizes []int
	var after *memory.ChunkCursor
	for i := 0; i < 50; i++ {
		page, err := s.ListPageWithContext(context.Background(), project, after, limit, memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational})
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Rows))
		for _, r := range page.Rows {
			ids = append(ids, r.ChunkID)
		}
		if page.Next == nil {
			break
		}
		after = page.Next
	}
	return ids, sizes
}

func TestIntegration_MemoryList_PagesKeysetWithTiesAndExcludesForgotten(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	s := memory.NewSearcher(memory.Config{}, repo, nil)
	project := persistence.GenerateID("itest-gh119-list")
	other := persistence.GenerateID("itest-gh119-other")
	t.Cleanup(func() { cleanupProjects(t, db, project, other) })
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	ids := seedListRows(t, db, project, 250, base, 3) // three rows share each created_at
	seedListRows(t, db, other, 20, base, 0)

	got, sizes := pageAll(t, s, project, 100)
	if fmt.Sprint(sizes) != "[100 100 50]" {
		t.Fatalf("page sizes %v, want [100 100 50]", sizes)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicate %s across pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 250 {
		t.Fatalf("listed %d distinct rows, want 250", len(seen))
	}

	ctx := context.Background()
	if n, err := repo.MarkRefutedByIDs(ctx, project, []string{ids[10]}, memory.RefuteRouteForgetCommand); err != nil || n != 1 {
		t.Fatalf("refute: %d %v", n, err)
	}
	if _, err := db.Exec(`UPDATE project_memory_chunks SET validation_status = 'superseded' WHERE id = $1`, ids[11]); err != nil {
		t.Fatal(err)
	}
	got, _ = pageAll(t, s, project, 100)
	for _, id := range got {
		if id == ids[10] || id == ids[11] {
			t.Fatalf("listing returned forgotten row %s", id)
		}
	}
	if len(got) != 248 {
		t.Fatalf("listed %d, want 248", len(got))
	}

	// GitHub #287: recent_memory's statement lists neither.
	if n, err := repo.MarkRefutedByIDs(ctx, project, []string{ids[249]}, memory.RefuteRouteMemoryCorrect); err != nil || n != 1 {
		t.Fatalf("refute newest: %d %v", n, err)
	}
	recent, err := repo.ListRecentChunksWithOptions(ctx, project, 50, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recent {
		if r.ChunkID == ids[249] || r.ChunkID == ids[10] || r.ChunkID == ids[11] {
			t.Fatalf("recent_memory listed forgotten row %s (#287)", r.ChunkID)
		}
	}
}

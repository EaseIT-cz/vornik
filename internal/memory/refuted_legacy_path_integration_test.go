//go:build integration

package memory_test

// GitHub #94 (2026-10-07, T4): the legacy path returned a refuted note. T4's
// integration helper used Searcher.Search on a Searcher with no epoch source
// and got back a note it had just refuted, because the six legacy search
// statements filtered expiry but not validation_status. Design 22, "No search
// path returns a refuted or superseded row". Behavioural, on the pgvector
// lane: every legacy entry point, including the two substring fallbacks
// (reached in production only when FTS errors), called directly.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

func chunkIDBySource(t *testing.T, db *sql.DB, projectID, source string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM project_memory_chunks WHERE project_id = $1 AND source_name = $2 LIMIT 1`,
		projectID, source).Scan(&id); err != nil {
		t.Fatalf("chunk for %s: %v", source, err)
	}
	return id
}

func hitIDs(res []memory.SearchResult) map[string]bool {
	out := make(map[string]bool, len(res))
	for _, r := range res {
		out[r.ChunkID] = true
	}
	return out
}

func TestIntegration_LegacySearchNeverReturnsRefutedOrSuperseded(t *testing.T) {
	db := openIngestRecallDB(t)
	pipeline, searcher := newIngestRecallStack(db) // no epoch source, no embedder
	repo := memory.NewRepository(db)
	ctx := context.Background()
	project := persistence.GenerateID("itest-gh94")
	t.Cleanup(func() { cleanupProjects(t, db, project) })

	const marker = "quuxwidget"
	notes := map[string]string{
		"refuted.md":    "The " + marker + " gateway listens on port 7001 for every regional relay node in the cluster today.",
		"superseded.md": "The " + marker + " gateway listens on port 7002 for every regional relay node in the cluster yesterday.",
		"live.md":       "The " + marker + " gateway listens on port 7003 for every regional relay node in the cluster tomorrow.",
	}
	for source, content := range notes {
		if st, _ := ingestForProject(t, db, pipeline, project, source, content); st.Admitted == 0 {
			t.Fatalf("%s not admitted: %+v", source, st)
		}
	}
	refuted := chunkIDBySource(t, db, project, "refuted.md")
	superseded := chunkIDBySource(t, db, project, "superseded.md")
	live := chunkIDBySource(t, db, project, "live.md")
	if n, err := repo.MarkRefutedByIDs(ctx, project, []string{refuted}, memory.RefuteRouteMemoryCorrect); err != nil || n != 1 {
		t.Fatalf("refute: %d %v", n, err)
	}
	// The state is what matters; the production writers are
	// SupersedeBySameSource, SupersedeDocument and SupersedeLegacyDocument.
	if _, err := db.Exec(`UPDATE project_memory_chunks SET validation_status = 'superseded' WHERE id = $1`, superseded); err != nil {
		t.Fatal(err)
	}

	window := time.Now().AddDate(0, 0, -1)
	calls := map[string]func() ([]memory.SearchResult, error){
		"Search": func() ([]memory.SearchResult, error) { return searcher.Search(ctx, project, marker, 20) },
		"SearchWithOptions with a date window": func() ([]memory.SearchResult, error) {
			return searcher.SearchWithOptions(ctx, project, marker, memory.SearchOptions{Limit: 20, FromDate: window})
		},
		"KeywordSearch": func() ([]memory.SearchResult, error) { return repo.KeywordSearch(ctx, project, marker, 20) },
		"substringSearch": func() ([]memory.SearchResult, error) {
			return memory.SubstringSearchForTest(ctx, repo, project, marker, 20)
		},
		"substringSearchTemporal": func() ([]memory.SearchResult, error) {
			return memory.SubstringSearchTemporalForTest(ctx, repo, project, marker, 20, window)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			res, err := call()
			if err != nil {
				t.Fatal(err)
			}
			got := hitIDs(res)
			if got[refuted] {
				t.Error("a refuted note was returned")
			}
			if got[superseded] {
				t.Error("a superseded note was returned")
			}
			if !got[live] {
				t.Errorf("the live note was not returned (%d hits)", len(res))
			}
		})
	}
}

//go:build integration

package memory_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/memory"
)

// Issue #330 (audit 2026-10-12): the classify backfill's only skip state was
// in memory, so a restarted daemon re-asked the paid model about every row it
// had already abstained on. A second backfiller over the same database is the
// restart; it must not call the model for the rows the first one saw abstain.
// Design: rag-ingest pipeline LLD, "Change, 2026-10-13 (#330)".
func TestClassifyBackfill_AttemptedMarkerSurvivesRestart_Integration(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("classify330-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id=$1`, project) })
	// Old enough to sort ahead of any other unclassified row in the shared
	// test database, and an unmapped producer_role so the model is asked.
	old := time.Now().UTC().AddDate(-30, 0, 0)
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%s-c%d", project, i)
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id,project_id,source_name,content,content_hash,created_at,content_class,producer_role,validation_status,lifecycle_state)
			VALUES ($1,$2,'x.md','ambiguous text',$1,$3,'unclassified','no-such-role-330','verified','published')`,
			id, project, old.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"unclassified"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	newBF := func() *memory.ClassifyBackfiller {
		return &memory.ClassifyBackfiller{
			Repo:       memory.NewRepository(db),
			Classifier: memory.NewClassifier(chat.NewClient(srv.URL, "k", "m"), "m", llmspend.Disabled()),
			Logger:     zerolog.Nop(),
		}
	}

	first, err := newBF().BackfillBatchAcrossProjects(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Skipped != 2 {
		t.Fatalf("first sweep: want both rows abstained and stamped, got %+v", first)
	}
	stamps := func() map[string]time.Time {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id, classify_attempted_at FROM project_memory_chunks
			WHERE project_id=$1 AND classify_attempted_at IS NOT NULL`, project)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]time.Time{}
		for rows.Next() {
			var id string
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				t.Fatal(err)
			}
			out[id] = at
		}
		return out
	}
	afterFirst := stamps()
	if len(afterFirst) != 2 {
		t.Fatalf("stamped rows = %d, want 2", len(afterFirst))
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls in first sweep = %d, want 2", calls.Load())
	}

	// The restart. Our rows are 30 years old, so without the marker they are
	// the head of the oldest-first queue and would be re-asked — and, the model
	// abstaining again, re-stamped with a later time. The shared test database
	// may hold other unclassified rows, so the check is on OUR rows' stamps,
	// not on the call count.
	time.Sleep(10 * time.Millisecond) // so a re-stamp would differ from the first
	if _, err := newBF().BackfillBatchAcrossProjects(ctx, 2); err != nil {
		t.Fatal(err)
	}
	afterRestart := stamps()
	for id, at := range afterFirst {
		if !afterRestart[id].Equal(at) {
			t.Errorf("%s re-asked after restart: stamp %v → %v", id, at, afterRestart[id])
		}
	}
}

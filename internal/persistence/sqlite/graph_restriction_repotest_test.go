package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// audit 2026-10-12 (GitHub #349): graph.SQLChunkLookup — wired over the daemon's
// handle on BOTH drivers — filtered lifecycle_state, a column the SQLite chunk
// table lacked, so ChunksMentioning errored on every call on SQLite.
func TestGraphChunkLookup_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGraphChunkLookupSuite(t, graphChunkLookupIDs(graph.NewSQLChunkLookup(db.DB)),
		seedChunkForSuite(db), setChunkStateForSuite(db), restrictChunkForSuite(db))
}

// audit 2026-10-12 (GitHub #350): the knowledge-graph Searcher screens through
// persistence.SQLRestrictionScreen; its SQL must answer on SQLite as on Postgres.
func TestRestrictionScreen_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunRestrictionScreenSuite(t, persistence.NewSQLRestrictionScreen(db.DB),
		seedChunkForSuite(db), restrictChunkForSuite(db))
}

// audit 2026-10-12 (GitHub #349): a database created before the column existed
// gets it from the additive reconciler, with the Postgres default, so its
// existing chunks read as published and stay visible to the graph lookup.
func TestChunkLifecycleState_ReconciledOnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.ExecContext(ctx, `ALTER TABLE project_memory_chunks DROP COLUMN lifecycle_state`); err != nil {
		t.Fatalf("drop column to simulate an older database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id, project_id, content, created_at)
		VALUES ('c-old', 'p-old', 'old', '2026-10-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var state string
	if err := db.QueryRowContext(ctx, `SELECT lifecycle_state FROM project_memory_chunks WHERE id = 'c-old'`).Scan(&state); err != nil {
		t.Fatalf("lifecycle_state not reconciled: %v", err)
	}
	if state != "published" {
		t.Errorf("existing chunk lifecycle_state = %q, want the Postgres default 'published'", state)
	}
	if _, err := db.ExecContext(ctx, `UPDATE project_memory_chunks SET lifecycle_state = 'bogus' WHERE id = 'c-old'`); err == nil {
		t.Error("the CHECK constraint (Postgres parity) did not reject an unknown lifecycle state")
	}
}

func graphChunkLookupIDs(l *graph.SQLChunkLookup) repotest.GraphChunkLookup {
	return func(ctx context.Context, projectID string, ids []string, scope string) ([]string, error) {
		rows, err := l.LookupChunks(ctx, projectID, ids, scope)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ChunkID)
		}
		return out, nil
	}
}

func setChunkStateForSuite(db *sqlite.DB) repotest.SetChunkState {
	return func(ctx context.Context, id, lifecycle, validation, scope string) error {
		_, err := db.ExecContext(ctx, `UPDATE project_memory_chunks
			SET lifecycle_state = ?, validation_status = ?, repo_scope = NULLIF(?, '')
			WHERE id = ?`, lifecycle, validation, scope, id)
		return err
	}
}

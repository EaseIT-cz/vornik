//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// audit 2026-10-12 (GitHub #349): the graph chunk lookup must behave identically
// on both drivers. Same suite as the SQLite file.
func TestGraphChunkLookup_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	l := graph.NewSQLChunkLookup(db.DB)
	lookup := func(ctx context.Context, projectID string, ids []string, scope string) ([]string, error) {
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
	set := func(ctx context.Context, id, lifecycle, validation, scope string) error {
		_, err := db.DB.ExecContext(ctx, `UPDATE project_memory_chunks
			SET lifecycle_state = $1, validation_status = $2, repo_scope = NULLIF($3, '')
			WHERE id = $4`, lifecycle, validation, scope, id)
		return err
	}
	repotest.RunGraphChunkLookupSuite(t, lookup, seedChunkForSuite(db), set, restrictChunkForSuite(db))
}

// audit 2026-10-12 (GitHub #350): the knowledge-graph Searcher's restriction
// screen. Same suite as the SQLite file.
func TestRestrictionScreen_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunRestrictionScreenSuite(t, persistence.NewSQLRestrictionScreen(db.DB),
		seedChunkForSuite(db), restrictChunkForSuite(db))
}

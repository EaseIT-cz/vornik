package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Retention on SQLite — GitHub #360, design
// https://docs.vornik.io §4.
// The statement-level contract runs on both drivers in
// repotest.RunRetentionSweepSuite; these pin the sweeper-side choices.

// The driver is identified from the handle: a modernc handle is SQLite, and
// anything else — lib/pq, or the sqlmock handle the Postgres-statement tests
// use — keeps the Postgres statements.
func TestNew_IdentifiesTheDriver(t *testing.T) {
	mockDB, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = mockDB.Close() })
	assert.False(t, New(mockDB, zerolog.Nop()).sqlite, "sqlmock handle must keep the Postgres statements")
	assert.False(t, New(nil, zerolog.Nop()).sqlite)
	assert.True(t, New(sqlitetest.Open(t, ":memory:").DB, zerolog.Nop()).sqlite)
}

// audit 2026-10-13 (GitHub #360): on a migrated SQLite database every global
// step failed at to_regclass, the chunk sweeps at FOR UPDATE, the quarantined-
// graph purge at a missing column and the eviction step at a missing table, so
// the daemon warned "retention sweep partially failed" on every tick. With
// every step enabled and nothing to prune, a sweep must now be clean.
func TestSQLite_EverySweepStepRunsClean(t *testing.T) {
	s := New(sqlitetest.Open(t, ":memory:").DB, zerolog.Nop())
	ctx := context.Background()
	policy := Resolve("p", Policy{MemoryChunksDays: 10, TaskMessagesDays: 10}, Policy{})
	global := GlobalPolicy{ResponseCacheDays: 1, EmbeddingCacheDays: 1}
	for _, preview := range []bool{true, false} {
		_, err := s.run(ctx, policy, preview)
		assert.NoError(t, err, "run preview=%v", preview)
		_, err = s.runGlobal(ctx, global, preview)
		assert.NoError(t, err, "runGlobal preview=%v", preview)
	}
}

// The chunk sweeps do not run on SQLite (row locks and Postgres-array graph
// settlement), but they still examine: an eligible chunk is reported as KEPT,
// in preview as well as in the sweep, and is not deleted.
func TestSQLite_ChunkSweepReportsKeptRows(t *testing.T) {
	db := sqlitetest.Open(t, ":memory:").DB
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s := New(db, zerolog.Nop(), WithClock(func() time.Time { return now }))
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id, project_id, created_at, expires_at)
		VALUES ('c-expired', 'p', $1, $2)`,
		persistence.SQLiteTime(now.AddDate(0, 0, -100)), persistence.SQLiteTime(now.AddDate(0, 0, -1)))
	require.NoError(t, err)

	for _, preview := range []bool{true, false} {
		counts, err := s.run(ctx, Resolve("p", Policy{}, Policy{}), preview)
		require.Error(t, err, "preview=%v", preview)
		assert.True(t, errors.Is(err, ErrPostgresOnly), "preview=%v: %v", preview, err)
		assert.Contains(t, err.Error(), "1 project_memory_chunks row(s)")
		assert.Contains(t, err.Error(), "KEPT")
		assert.Zero(t, counts.MemoryExpired, "preview=%v must not promise a deletion", preview)
	}
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_memory_chunks WHERE id = 'c-expired'`).Scan(&n))
	assert.Equal(t, 1, n)
}

// The quarantined-graph purge returns zero on SQLite because nothing can be
// parked there — checked, not assumed: if the graph gains quarantine columns
// on SQLite, the step refuses instead of reporting a clean zero
// (review-20261013-fa9b finding 3).
func TestSQLite_QuarantinePurgeRefusesOnceColumnsExist(t *testing.T) {
	db := sqlitetest.Open(t, ":memory:").DB
	s := New(db, zerolog.Nop())
	ctx := context.Background()
	n, err := s.purgeQuarantinedGraph(ctx, Policy{ProjectID: "p"}, time.Now(), false)
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = db.ExecContext(ctx, `ALTER TABLE knowledge_entities ADD COLUMN quarantined_at TEXT`)
	require.NoError(t, err)
	_, err = s.purgeQuarantinedGraph(ctx, Policy{ProjectID: "p"}, time.Now(), true)
	assert.ErrorIs(t, err, ErrPostgresOnly)
}

func TestSQLite_MemoryApprovalRefuses(t *testing.T) {
	s := New(sqlitetest.Open(t, ":memory:").DB, zerolog.Nop())
	_, err := s.MemoryApprovalRequests(context.Background(), "p", 0, time.Now())
	assert.ErrorIs(t, err, ErrPostgresOnly)
	_, err = s.ApplyMemoryApproval(context.Background(), persistence.AgentApprovalRequestRow{}, 0, time.Now())
	assert.ErrorIs(t, err, ErrPostgresOnly)
}

// The SQLite artifact delete binds an IN list in batches; more ids than one
// batch must all go.
func TestSQLite_ArtifactDeleteBatches(t *testing.T) {
	db := sqlitetest.Open(t, ":memory:").DB
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s := New(db, zerolog.Nop(), WithClock(func() time.Time { return now }))
	ctx := context.Background()
	const rows = sqliteDeleteBatch + 7
	for i := 0; i < rows; i++ {
		_, err := db.ExecContext(ctx, `INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, created_at)
			VALUES ($1, 'p', 'a', 'OUTPUT', '', $2)`, "a-"+time.Duration(i).String(), persistence.SQLiteTime(now.AddDate(0, 0, -90)))
		require.NoError(t, err)
	}
	files, n, err := s.pruneArtifacts(ctx, "p", now.AddDate(0, 0, -60), "", false)
	require.NoError(t, err)
	assert.Zero(t, files)
	assert.Equal(t, rows, n)
	var left int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts`).Scan(&left))
	assert.Zero(t, left)
}

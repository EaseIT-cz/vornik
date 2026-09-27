package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// oldExecutionQualityScoresDDL is the table as it shipped before `unscorable`
// (agent-quality-benchmark design, amendment 2026-09-26), verbatim.
const oldExecutionQualityScoresDDL = `CREATE TABLE IF NOT EXISTS execution_quality_scores (
    execution_id       TEXT PRIMARY KEY REFERENCES executions(id) ON DELETE CASCADE,
    project_id         TEXT NOT NULL,
    task_id            TEXT NOT NULL,
    workflow_id        TEXT NOT NULL,
    workflow_revision  TEXT NOT NULL,
    scorer_version     TEXT NOT NULL,
    scoring_policy_sha TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL CHECK (status IN ('scored','missing_contract','invalid_evidence','not_applicable')),
    score              REAL,
    passed_case_count  INTEGER NOT NULL DEFAULT 0,
    pinned_case_count  INTEGER NOT NULL DEFAULT 0,
    diagnostic         TEXT NOT NULL DEFAULT '',
    case_evidence      TEXT NOT NULL DEFAULT '[]',
    recorded_at        TEXT NOT NULL,
    CHECK ((status = 'not_applicable' AND score IS NULL) OR
           (status <> 'not_applicable' AND score IS NOT NULL)),
    CHECK (score IS NULL OR (score >= 0 AND score <= 1)),
    CHECK (passed_case_count >= 0 AND pinned_case_count >= passed_case_count)
)`

// An existing SQLite database keeps the table it was created with, because
// schemaSQL is CREATE TABLE IF NOT EXISTS. Before the rebuild, every local and
// Community install would have refused `unscorable` forever; the incident was
// the slow-hardware bench arm's reconciler retrying one such row every 30 s.
func TestMigrate_RebuildsExecutionQualityScoresOnAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // the PRAGMA below is per connection

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Replace the table with the shape an older database has, holding one row
	// of each old status.
	for _, stmt := range []string{
		`PRAGMA foreign_keys = OFF`,
		`DROP TABLE execution_quality_scores`,
		oldExecutionQualityScoresDDL,
		`INSERT INTO execution_quality_scores (execution_id, project_id, task_id, workflow_id, workflow_revision, scorer_version, status, score, recorded_at) VALUES
			('e1','p','t','w','r','v1','scored',0.5,'2026-09-01T00:00:00Z'),
			('e2','p','t','w','r','v1','missing_contract',0,'2026-09-01T00:00:00Z'),
			('e3','p','t','w','r','v1','invalid_evidence',0,'2026-09-01T00:00:00Z'),
			('e4','p','t','w','r','v1','not_applicable',NULL,'2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed old schema (%s): %v", strings.SplitN(stmt, "(", 2)[0], err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO execution_quality_scores (execution_id, project_id, task_id, workflow_id, workflow_revision, scorer_version, status, score, recorded_at)
		VALUES ('e5','p','t','w','r','v1','unscorable',NULL,'2026-09-01T00:00:00Z')`); err == nil {
		t.Fatal("precondition: the old table must refuse unscorable")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on an existing database: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_quality_scores`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("rows after rebuild = %d (%v), want the 4 old rows", n, err)
	}
	var score float64
	if err := db.QueryRowContext(ctx, `SELECT score FROM execution_quality_scores WHERE execution_id='e1'`).Scan(&score); err != nil || score != 0.5 {
		t.Fatalf("a row's values must survive the rebuild: %v %v", score, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO execution_quality_scores (execution_id, project_id, task_id, workflow_id, workflow_revision, scorer_version, status, score, recorded_at)
		VALUES ('e5','p','t','w','r','v1','unscorable',NULL,'2026-09-01T00:00:00Z')`); err != nil {
		t.Fatalf("after the rebuild the table must accept unscorable: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO execution_quality_scores (execution_id, project_id, task_id, workflow_id, workflow_revision, scorer_version, status, score, recorded_at)
		VALUES ('e6','p','t','w','r','v1','unscorable',0,'2026-09-01T00:00:00Z')`); err == nil {
		t.Fatal("unscorable with a number must still be refused")
	}
	var idx int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='execution_quality_scores' AND name LIKE 'idx_%'`).Scan(&idx); err != nil || idx != 2 {
		t.Fatalf("the table's indexes must be recreated, got %d (%v)", idx, err)
	}
	// Idempotent: a third Migrate finds the marker and leaves the table alone.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("idempotent Migrate: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_quality_scores`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("rows after an idempotent Migrate = %d (%v), want 5", n, err)
	}
}

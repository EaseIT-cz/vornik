package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// oldMemoryVerificationEventsDDL is the table as migration 234's SQLite half
// shipped it, before the demote kind (GitHub #550), verbatim.
const oldMemoryVerificationEventsDDL = `CREATE TABLE memory_verification_events (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('backfill','sweep','restore')),
    actor             TEXT NOT NULL,
    reason            TEXT NOT NULL DEFAULT '',
    restores_event_id TEXT,
    examined          INTEGER NOT NULL DEFAULT 0,
    unchanged         INTEGER NOT NULL DEFAULT 0,
    verified          INTEGER NOT NULL DEFAULT 0,
    deverified        INTEGER NOT NULL DEFAULT 0,
    skipped           INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL
)`

// GitHub #550 (design 22, amendment 2026-11-15, review-20261115-9143 F5): an
// existing database learns the demote kind by a table rebuild, and the events
// table is not a leaf: memory_verification_backup references it. The backup
// rows, their event ids and the reference must all survive, and the columns
// the demote writes (chunk confidence, the backup's confidence pair) must
// exist on a database that predates them.
func TestSQLite_VerificationEventsRebuildKeepsBackup_550(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Turn it into a database from before #550.
	for _, stmt := range []string{
		`ALTER TABLE memory_verification_backup DROP COLUMN prior_confidence`,
		`ALTER TABLE memory_verification_backup DROP COLUMN new_confidence`,
		`ALTER TABLE project_memory_chunks DROP COLUMN confidence`,
		`DROP TABLE memory_verification_events`,
		oldMemoryVerificationEventsDDL,
		`INSERT INTO memory_verification_events (id, project_id, kind, actor, created_at)
			VALUES ('ev1', 'p', 'backfill', 'operator', '2026-10-21T00:00:00Z')`,
		`INSERT INTO memory_verification_backup (event_id, chunk_id, project_id, prior_status, new_status)
			VALUES ('ev1', 'c1', 'p', 'verified', 'unverified')`,
		`INSERT INTO project_memory_chunks (id, project_id, content, content_hash, created_at, validation_status)
			VALUES ('c1', 'p', 'x', 'h', '2026-10-21T00:00:00Z', 'unverified')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed old schema (%s): %v", strings.SplitN(stmt, "(", 2)[0], err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_verification_events (id, project_id, kind, actor, created_at)
		VALUES ('ev0', 'p', 'demote', 'operator', '2026-11-15T00:00:00Z')`); err == nil {
		t.Fatal("precondition: the old table must refuse demote")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on an existing database: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_backup b
		JOIN memory_verification_events e ON e.id = b.event_id WHERE e.id = 'ev1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup rows joined to their event after the rebuild = %d (%v), want 1", n, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_verification_events (id, project_id, kind, actor, created_at)
		VALUES ('ev2', 'p', 'demote', 'operator', '2026-11-15T00:00:00Z')`); err != nil {
		t.Fatalf("the rebuilt table refuses demote: %v", err)
	}
	var backupDDL string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='memory_verification_backup'`).Scan(&backupDDL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backupDDL, "REFERENCES memory_verification_events(id)") || strings.Contains(backupDDL, "_rebuild") {
		t.Fatalf("the backup's reference after the rebuild: %s", backupDDL)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memory_verification_backup SET prior_confidence = 0.9, new_confidence = 0.3 WHERE event_id = 'ev1'`); err != nil {
		t.Fatalf("the backup's confidence pair is missing: %v", err)
	}
	var conf float64
	if err := db.QueryRowContext(ctx, `SELECT confidence FROM project_memory_chunks WHERE id = 'c1'`).Scan(&conf); err != nil || conf != 0.5 {
		t.Fatalf("an existing chunk's confidence = %v (%v), want the default 0.5", conf, err)
	}
}

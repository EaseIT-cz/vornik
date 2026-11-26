package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #920 (Postgres migration 265): an existing database's tasks table
// learns creation_source HANDOVER through the table rebuild (SQLite cannot
// ALTER a CHECK); old rows and the indexes survive.
func TestMigrate_RebuildsTasksForHandOver_920(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(tasksTableSQL, "'CHECKPOINT', 'FORK', 'HANDOVER')", "'CHECKPOINT', 'FORK')", 1)
	if old == tasksTableSQL {
		t.Fatal("precondition: the DDL names HANDOVER")
	}
	insert := func(id, source string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO tasks (id, project_id, creation_source, created_at, updated_at)
			VALUES (?, 'p', ?, '2026-11-25T00:00:00Z', '2026-11-25T00:00:00Z')`, id, source)
		return err
	}
	for _, stmt := range []string{`DROP TABLE tasks`, old} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := insert("t_old", "SCHEDULED"); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := insert("t_h0", "HANDOVER"); err == nil {
		t.Fatal("precondition: the old table must refuse HANDOVER")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := insert("t_h", "HANDOVER"); err != nil {
		t.Fatalf("after the rebuild HANDOVER must insert: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = 't_old'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the old row did not survive: n=%d err=%v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tasks_project'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("idx_tasks_project missing after the rebuild: n=%d err=%v", n, err)
	}
}

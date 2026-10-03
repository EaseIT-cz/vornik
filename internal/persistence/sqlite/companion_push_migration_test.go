package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Migration 203 on SQLite (broker write-actions design §7a,
// review-20260930-5e5d F1/F5): on an existing database the reconciler adds
// pushed_state and backfills it once — a task config takes its task's
// status, a pending action and a staged action of a COMPLETED task become
// pending_approval, a staged action of an unfinished task stays NULL — so an
// upgrade pushes no backlog.
func TestMigration203_ExistingRowsAreMarkedPushed(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Back to the pre-203 shape.
	exec(`ALTER TABLE a2a_push_configs DROP COLUMN pushed_state`)
	exec(`ALTER TABLE broker_actions DROP COLUMN pushed_state`)
	exec(`INSERT INTO tasks (id, project_id, status, creation_source, created_at, updated_at) VALUES ('t203', 'p', 'COMPLETED', 'COMPANION', '2026-09-30T00:00:00Z', '2026-09-30T00:00:00Z')`)
	exec(`INSERT INTO a2a_push_configs (task_id, url, created_at) VALUES ('t203', 'https://h.example/x', '2026-09-30T00:00:00Z')`)
	for i, st := range []string{"pending", "staged", "executed"} {
		exec(`INSERT INTO broker_actions (action_id, project_id, task_id, api_key_id, workflow_id, action_kind, tool, args_json, args_sha256, status, created_at, expires_at)
			VALUES (?, 'p', 't203', '', 'w', ?, 'mcp__w__t', '{}', 'h', ?, '2026-09-30T00:00:00Z', '2026-10-01T00:00:00Z')`,
			"a-"+st, string(rune('a'+i)), st)
	}
	exec(`INSERT INTO tasks (id, project_id, status, creation_source, created_at, updated_at) VALUES ('t203run', 'p', 'RUNNING', 'COMPANION', '2026-09-30T00:00:00Z', '2026-09-30T00:00:00Z')`)
	exec(`INSERT INTO broker_actions (action_id, project_id, task_id, api_key_id, workflow_id, action_kind, tool, args_json, args_sha256, status, created_at, expires_at)
		VALUES ('a-staged-running', 'p', 't203run', '', 'w', 'k', 'mcp__w__t', '{}', 'h', 'staged', '2026-09-30T00:00:00Z', '2026-10-01T00:00:00Z')`)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var cfg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT pushed_state FROM a2a_push_configs WHERE task_id = 't203'`).Scan(&cfg); err != nil || cfg.String != "COMPLETED" {
		t.Fatalf("config pushed_state = %v (%v)", cfg, err)
	}
	for st, want := range map[string]sql.NullString{
		"pending":        {String: "pending_approval", Valid: true},
		"staged":         {String: "pending_approval", Valid: true}, // of a COMPLETED task (review-20260930-c6b5 F1)
		"staged-running": {},                                        // never visible; pushed once when promoted
		"executed":       {String: "executed", Valid: true},
	} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT pushed_state FROM broker_actions WHERE action_id = ?`, "a-"+st).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: pushed_state = %v, want %v", st, got, want)
		}
	}
	// A second Migrate does not re-run the backfill (the column exists).
	exec(`UPDATE broker_actions SET pushed_state = 'approved' WHERE action_id = 'a-pending'`)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var again string
	_ = db.QueryRowContext(ctx, `SELECT pushed_state FROM broker_actions WHERE action_id = 'a-pending'`).Scan(&again)
	if again != "approved" {
		t.Fatalf("backfill re-ran on an up-to-date database: %q", again)
	}
}

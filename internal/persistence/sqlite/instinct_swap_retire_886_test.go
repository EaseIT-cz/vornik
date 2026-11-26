package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #886 S4: the tool-swap instincts the old rule learned from words in
// tool output are retired once (Postgres migration 263's SQLite twin); other
// instincts, and swap instincts learned after the migration, are left alone.
func TestMigrate_RetiresOldToolSwapInstinctsOnce_886(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	insert := func(id, domain, action, status string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO instincts (id, scope, project_id, domain, trigger_key, trigger_json, action, confidence, status, source, created_at, updated_at, last_seen_at)
			VALUES (?, 'project', 'p', ?, ?, '{}', ?, 0.9, ?, 'observer', '2026-11-22T00:00:00Z', '2026-11-22T00:00:00Z', '2026-11-22T00:00:00Z')`, id, domain, "tk_"+id, action, status); err != nil {
			t.Fatal(err)
		}
	}
	status := func(id string) string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, `SELECT status FROM instincts WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	insert("swap-active", "workflow", "swapping off tool file_read resolved the step", "active")
	insert("swap-candidate", "workflow", "swapping off tool git_show resolved the step", "candidate")
	insert("budget", "cost", "role coder is over-provisioned (finishes well under its tool budget)", "active")
	insert("retry", "recovery", "retrying the step resolved the output_cap failure", "candidate")
	// A database that ran Migrate before this change: the marker is absent.
	if _, err := db.ExecContext(ctx, `DELETE FROM vornik_data_migrations WHERE name LIKE '%tool-swap%'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// updated_at keeps the fixed-width layout every SQLite timestamp uses,
	// so text ordering stays correct (sqlite timestamp ordering design D1).
	var upd string
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM instincts WHERE id = 'swap-active'`).Scan(&upd); err != nil {
		t.Fatal(err)
	}
	if len(upd) != len("2006-01-02T15:04:05.000000000Z") {
		t.Errorf("updated_at %q is not the fixed-width layout", upd)
	}
	for id, want := range map[string]string{"swap-active": "retired", "swap-candidate": "retired", "budget": "active", "retry": "candidate"} {
		if got := status(id); got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
	// A swap instinct the fixed rule learns later survives the next Migrate.
	insert("swap-new", "workflow", "swapping off tool file_write resolved the step", "candidate")
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := status("swap-new"); got != "candidate" {
		t.Errorf("a later swap instinct was retired again: %q", got)
	}
}

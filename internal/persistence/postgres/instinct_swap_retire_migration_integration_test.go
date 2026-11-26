//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #886 S4: Postgres migration 263 retires the tool-swap instincts the
// old rule learned from words in tool output, and nothing else. `go test ./...`
// is the SQLite lane (its twin is TestMigrate_RetiresOldToolSwapInstinctsOnce_886);
// this is the Postgres half.
//
// Run with: make test-integration, or
// go test -tags=integration ./internal/persistence/postgres/... -run Migration263
func TestIntegrationMigration263_RetiresOldToolSwapInstincts(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 263 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration 263 not found")
	}
	// The integration database persists between runs: start from, and leave,
	// no rows of this test's own.
	clear := func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM instincts WHERE id LIKE 'm263-%'`); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
	insert := func(id, domain, action, status string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO instincts (id, scope, project_id, domain, trigger_key, trigger_json, action, confidence, status, source)
			VALUES ($1, 'project', 'p', $2, $3, '{}', $4, 0.9, $5, 'observer')`, id, domain, "tk_"+id, action, status); err != nil {
			t.Fatal(err)
		}
	}
	insert("m263-swap-active", "workflow", "swapping off tool file_read resolved the step", "active")
	insert("m263-swap-candidate", "workflow", "swapping off tool git_show resolved the step", "candidate")
	insert("m263-budget", "budget", "role coder is over-provisioned (finishes well under its tool budget)", "active")
	insert("m263-retry", "recovery", "retrying the step resolved the output_cap failure", "candidate")

	// Rows written before the migration ran: apply its Up as the runner does.
	if _, err := db.ExecContext(ctx, up); err != nil {
		t.Fatalf("migration 263 Up: %v", err)
	}
	for id, want := range map[string]string{
		"m263-swap-active": "retired", "m263-swap-candidate": "retired",
		"m263-budget": "active", "m263-retry": "candidate",
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT status FROM instincts WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
}

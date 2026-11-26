//go:build integration

package postgres

// GitHub #887 (instinct tool-budget seam design, amendment 2026-11-23): every
// listed instinct domain inserts on Postgres too, and the column comment
// names the list rather than five values. Found at: unit, while writing the
// SQLite test of #886, where budget was refused.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestIntegrationInstinctsAcceptEveryDomain_887(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	clear := func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM instincts WHERE id LIKE 'd887-%'`); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
	for _, d := range persistence.InstinctDomains() {
		if _, err := db.ExecContext(ctx, `INSERT INTO instincts (id, scope, project_id, domain, trigger_key, trigger_json, action, confidence, status, source)
			VALUES ($1, 'project', 'p887', $2, $3, '{}', 'act', 0.5, 'candidate', 'observer')`, "d887-"+d, d, "tk-d887-"+d); err != nil {
			t.Errorf("domain %q refused: %v", d, err)
		}
	}
	var comment string
	if err := db.QueryRowContext(ctx, `SELECT col_description('instincts'::regclass, (SELECT attnum FROM pg_attribute WHERE attrelid = 'instincts'::regclass AND attname = 'domain'))`).Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment, "InstinctDomains") {
		t.Errorf("column comment %q should name persistence.InstinctDomains()", comment)
	}
}

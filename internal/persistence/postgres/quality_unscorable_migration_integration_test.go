//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Migration 199 (agent-quality-benchmark design, amendment 2026-09-26):
// execution_quality_scores learns `unscorable`. Incident: the slow-hardware
// bench arm, where the refused row was retried every 30 s, forever.
//
// Run on a POPULATED table: the ADD CONSTRAINT re-scans every row, and the
// design's claim that it cannot fail rests on the new CHECKs being supersets
// of the old. The old table is rebuilt in a scratch schema, so the migration's
// own SQL runs against real rows without touching the shared database.
//
// Run with: make test-integration, or
// go test -tags=integration ./internal/persistence/postgres/... -run Migration199
func TestIntegrationMigration199_PopulatedTableAcceptsUnscorable(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 199 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration 199 not found")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	exec := func(q string) error { _, err := conn.ExecContext(ctx, q); return err }
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS m199_test CASCADE`) })

	for _, q := range []string{
		`DROP SCHEMA IF EXISTS m199_test CASCADE`,
		`CREATE SCHEMA m199_test`,
		`SET search_path TO m199_test`,
		// The shape the table was created with before 199, minus the foreign key
		// (irrelevant to the CHECKs, and it would need an executions table).
		// The CHECKs are UNNAMED on purpose: the original migration left them
		// unnamed, production's constraints carry Postgres's generated names
		// (verified on the reference host 2026-09-26), and migration 199 drops
		// them BY those names. Naming them here would hide a mismatch.
		`CREATE TABLE execution_quality_scores (
			execution_id TEXT PRIMARY KEY, status TEXT NOT NULL CHECK (status IN ('scored','missing_contract','invalid_evidence','not_applicable')),
			score DOUBLE PRECISION,
			CHECK ((status = 'not_applicable' AND score IS NULL) OR (status <> 'not_applicable' AND score IS NOT NULL)),
			CHECK (score IS NULL OR (score >= 0 AND score <= 1)))`,
		`INSERT INTO execution_quality_scores VALUES ('e1','scored',0.5),('e2','missing_contract',0),('e3','invalid_evidence',0),('e4','not_applicable',NULL)`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(q)[0], err)
		}
	}
	if err := exec(`INSERT INTO execution_quality_scores VALUES ('e5','unscorable',NULL)`); err == nil {
		t.Fatal("precondition: the old table refuses unscorable")
	}
	if err := exec(up); err != nil {
		t.Fatalf("migration 199 on a populated table: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_quality_scores`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("rows after migration = %d (%v), want 4", n, err)
	}
	if err := exec(`INSERT INTO execution_quality_scores VALUES ('e5','unscorable',NULL)`); err != nil {
		t.Fatalf("after 199 unscorable with a NULL score is accepted: %v", err)
	}
	if err := exec(`INSERT INTO execution_quality_scores VALUES ('e6','unscorable',0)`); err == nil {
		t.Fatal("unscorable with a number must be refused")
	}
	if err := exec(`INSERT INTO execution_quality_scores VALUES ('e7','bogus',0.1)`); err == nil {
		t.Fatal("a status outside the list must still be refused")
	}
}

//go:build integration

package postgres

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// GitHub #374 (found at: integration — the gate stamp's Postgres lane,
// 2026-10-14). `make test-integration` ran every Postgres test against one
// long-lived vornik_integration_test, shared by every run and every worktree
// on the host. Rows piled up (about 3,500 a run; 63,500 by 2026-10-15) and
// tests that read "the first N rows" failed once the leftovers reached N:
// TestDataSubjectLedger_PostgresContract (ListRequests(ctx, 200, 0) at 235
// leftovers) and TestIntegration_EmbedQueue_ClaimDoesNotRemoveTheRow
// (DequeueEmbedBatch(10) claimed ten leftovers). The lane now recreates the
// database at the start of every run (scripts/integration-db-lane.sh;
// storage-abstraction design, change 2026-10-15).
//
// This test is the lane-level proof. It keeps a canary row per run, in a
// schema of its own so no table-set or schema-parity check sees it, and fails
// when the database already holds a row from another run: on a database that
// is not recreated, the second run fails. The run id is the lane's
// VORNIK_TEST_INTEGRATION_RUN_ID, or a per-process random id when the package is
// run by hand (so two hand runs against a database nobody recreated fail
// here, which is the condition #374 is about).
func TestIntegration_LaneStartsFromAFreshDatabase(t *testing.T) {
	db := newIntegrationDB(t)
	runID := os.Getenv("VORNIK_TEST_INTEGRATION_RUN_ID")
	if runID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		runID = "process-" + hex.EncodeToString(b[:])
	}
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS integration_lane`,
		`CREATE TABLE IF NOT EXISTS integration_lane.run_canary (
			run_id      TEXT NOT NULL,
			recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("canary setup: %v", err)
		}
	}
	var other, at string
	err := db.DB.QueryRow(`SELECT run_id, recorded_at::text FROM integration_lane.run_canary
		WHERE run_id <> $1 ORDER BY recorded_at LIMIT 1`, runID).Scan(&other, &at)
	switch {
	case err == nil:
		t.Fatalf("the integration database holds rows from an earlier run (%s, at %s): the lane did not start "+
			"from a fresh database, so fixed-page reads can see that run's leftovers (#374). Run the lane with "+
			"`make test-integration`, which recreates vornik_integration_test, or recreate it by hand", other, at)
	case !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("canary read: %v", err)
	}
	if _, err := db.DB.Exec(`INSERT INTO integration_lane.run_canary (run_id) VALUES ($1)`, runID); err != nil {
		t.Fatalf("canary write: %v", err)
	}
}

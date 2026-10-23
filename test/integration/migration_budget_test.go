//go:build integration
// +build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #424: make test-integration died in TestMain with
// "pq: canceling statement due to user request" at migrations 13, 141 and
// 202 on three runs, leaving the schema half built. The canceller was
// applyTestMigrations' fixed 60 s context: since #374 recreates the
// database every run, the whole chain replays inside it, and with other
// gate runs loading the host it ran out. The error said nothing of a
// context. These tests make that interleaving deterministic: a budget that
// ends while a migration's statement runs on the real server.

// budgetTestVersion is a migration version no real migration uses, so the
// cancelled migration below can never collide with the chain.
const budgetTestVersion = 2_000_000_424

func TestIntegration_MigrationBudget_NamesTheCanceller(t *testing.T) {
	db := connectDB(t)
	// Closing the pool also ends the session that holds the runner's
	// advisory lock: Run's deferred unlock uses the ended context.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM migrations WHERE version = $1`, budgetTestVersion)
		_ = db.Close()
	})

	runner := persistence.NewMigrationRunnerFor(db, []persistence.Migration{
		// Version 1 is what syncBootstrapSchema keys on; it is already
		// recorded in the lane database, so only the slow one is pending.
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: budgetTestVersion, Name: "slow_424", Up: "SELECT pg_sleep(30)"},
	})
	start := time.Now()
	err := runMigrations(runner, 300*time.Millisecond)
	if err == nil {
		t.Fatal("runMigrations returned nil; want the budget to cancel the slow migration")
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("runMigrations took %s; the 300ms budget did not cancel the statement", time.Since(start))
	}
	msg := err.Error()
	// The text #424's runs printed: the server answering pq's cancel request.
	if !strings.Contains(msg, "canceling statement due to user request") {
		t.Errorf("error %q does not carry the server's cancel answer; the seam did not reproduce #424", msg)
	}
	// The fix: the error names who cancelled it.
	if !strings.Contains(msg, "test/integration migration budget of 300ms") {
		t.Errorf("error %q does not name the budget that cancelled it (#424)", msg)
	}
	if !strings.Contains(msg, "0 of 1 pending migrations applied") {
		t.Errorf("error %q does not say how far the chain got (#424)", msg)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %q", msg)
	}

	var recorded bool
	if qerr := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM migrations WHERE version = $1)`, budgetTestVersion).Scan(&recorded); qerr != nil {
		t.Fatalf("check migrations: %v", qerr)
	}
	if recorded {
		t.Error("the cancelled migration was recorded as applied")
	}
	assertSchemaUsable(t, db)
}

// The budget is the package's own -test.timeout minus a reserve for the
// tests (half, at most 2 minutes), not a fixed guess about host load
// (#424: 60 s for any timeout).
func TestIntegration_MigrationBudget_FollowsThePackageTimeout(t *testing.T) {
	cases := []struct {
		timeout, want time.Duration
	}{
		{10 * time.Minute, 8 * time.Minute},
		{30 * time.Minute, 28 * time.Minute},
		{time.Minute, 30 * time.Second},
		{0, 0}, // no -timeout: no deadline
	}
	for _, c := range cases {
		if got := migrationBudget(c.timeout); got != c.want {
			t.Errorf("migrationBudget(%s) = %s, want %s", c.timeout, got, c.want)
		}
	}
}

// assertSchemaUsable checks the test left the lane's schema as it found it:
// the advisory lock released and a table the suite needs still present.
func assertSchemaUsable(t *testing.T, db *sql.DB) {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT to_regclass('public.cluster_nodes') IS NOT NULL`).Scan(&ok); err != nil || !ok {
		t.Errorf("cluster_nodes missing after the budget test (ok=%v err=%v)", ok, err)
	}
}

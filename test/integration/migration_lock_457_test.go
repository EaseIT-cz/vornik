//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #457 (storage-abstraction design, "The migration replay's budget",
// fixed 2026-11-08). Found at review, observable in a long-lived process: Run
// took its session-scoped advisory lock through the pool and unlocked with
// its own context. When that context ended between two statements, the next
// statement failed without touching the lock's connection, the unlock was
// skipped, and the connection went back to the pool still holding the lock.
// (A context ending DURING a statement hid it: lib/pq marks that connection
// bad and database/sql discards it, which ends the session.) The seam ends
// Run's context right after the lock is taken.
func TestIntegration_MigrationLock457_CancelledRunReleasesTheLock(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	other := connectDB(t)
	t.Cleanup(func() { _ = other.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := persistence.NewMigrationRunnerFor(db, []persistence.Migration{{Version: 1, Name: "initial", Up: "SELECT 1"}}).
		WithAfterLockHook(cancel)
	if err := runner.Run(ctx); err == nil {
		t.Fatal("Run must fail once its context has ended")
	}

	// From a separate session: is the migration lock still held in this
	// database? Key 0x73776D646D696772 is classid 0x73776D64, objid 0x6D696772.
	var held int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := other.QueryRow(`SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND granted
			  AND classid = 1937206628 AND objid = 1835624306
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if held != 0 {
		t.Fatalf("a cancelled Run left the migration advisory lock held (%d holder(s)); the next Run from another process would block on it", held)
	}
}

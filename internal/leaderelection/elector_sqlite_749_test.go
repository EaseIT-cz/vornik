package leaderelection

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #749 item 3 (2026-11-13 batch review of #199): on SQLite the elector
// can now lose leadership, because Renew answers false once a rival holds the
// row. Only the repository suite pinned that; the fake-repository elector
// tests cannot tell the old SQLite Renew (always true, changed nothing) from
// the fixed one. This drives the elector over the real SQLite repository: a
// renewal keeps the lease, and after a rival takes it the next renewal
// demotes this daemon. Red on the pre-#199 Renew.
//
// Found at: review (2026-11-13 batch review).
// Regression test at: unit (elector over the real SQLite repository).
func TestElector749_SQLiteRenewMissDemotes(t *testing.T) {
	ctx := sqlitetest.Context(t)
	repo := sqlite.NewLeaderLockRepository(sqlitetest.Memory(t).DB)
	const ttl = 10 * time.Second
	e := New(repo, "w", "us", ttl, zerolog.Nop())

	e.tryAcquireOrRenew(ctx)
	if !e.IsLeader() {
		t.Fatalf("first acquire on an empty table must lead; lastErr=%v", e.LastError())
	}
	e.tryAcquireOrRenew(ctx) // the Renew branch, while we still hold the row
	if !e.IsLeader() {
		t.Fatalf("a renewal by the holder must keep the lease; lastErr=%v", e.LastError())
	}

	// A rival takes the row an hour later (the now argument), when our 10 s
	// lease has expired from its point of view.
	ok, _, err := repo.Acquire(ctx, "w", "them", time.Now().Add(time.Hour), ttl)
	if err != nil || !ok {
		t.Fatalf("rival acquire after expiry: ok=%v err=%v", ok, err)
	}

	e.tryAcquireOrRenew(ctx)
	if e.IsLeader() {
		t.Fatal("Renew by a non-holder must demote the elector; it still reports leader")
	}
	if e.LastError() != nil {
		t.Fatalf("a lost lease is a refusal, not an error: %v", e.LastError())
	}
}

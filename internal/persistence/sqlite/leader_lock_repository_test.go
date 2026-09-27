package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestLeaderLock_EpochBehavior is the in-memory SQLite behavioral
// test for the epoch fence token:
//
//  1. First acquire → epoch 1.
//  2. A DIFFERENT holder takes over after the row expires → epoch 2.
//  3. The same holder renews → epoch stays 2.
func TestLeaderLock_EpochBehavior(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewLeaderLockRepository(db.DB)
	ctx := context.Background()

	base := time.Now().UTC()

	// ── Step 1: first acquire (host-a acquires fresh lock) ──────
	ok, epoch, err := repo.Acquire(ctx, "sweeper", "host-a", base, 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !ok {
		t.Fatalf("first acquire: want true, got false")
	}
	if epoch != 1 {
		t.Errorf("first acquire epoch = %d, want 1", epoch)
	}

	// ── Step 2: takeover — host-b acquires after host-a's lock ──
	// expires (set now to past the expiry).
	afterExpiry := base.Add(31 * time.Second)
	ok, epoch, err = repo.Acquire(ctx, "sweeper", "host-b", afterExpiry, 30*time.Second)
	if err != nil {
		t.Fatalf("takeover acquire: %v", err)
	}
	if !ok {
		t.Fatalf("takeover acquire: want true, got false")
	}
	if epoch != 2 {
		t.Errorf("takeover epoch = %d, want 2 (bumped)", epoch)
	}

	// ── Step 3: same-holder renew (host-b renews its own lock) ──
	renewTime := afterExpiry.Add(5 * time.Second)
	ok, epoch, err = repo.Acquire(ctx, "sweeper", "host-b", renewTime, 30*time.Second)
	if err != nil {
		t.Fatalf("renew acquire: %v", err)
	}
	if !ok {
		t.Fatalf("renew acquire: want true, got false")
	}
	if epoch != 2 {
		t.Errorf("renew epoch = %d, want 2 (preserved)", epoch)
	}
}

// TestLeaderLock_GetScansEpoch: Get must return the persisted
// epoch so diagnostics and the admin UI can surface the fence token.
func TestLeaderLock_GetScansEpoch(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewLeaderLockRepository(db.DB)
	ctx := context.Background()

	now := time.Now().UTC()
	if _, _, err := repo.Acquire(ctx, "sweeper", "host-a", now, 30*time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	row, err := repo.Get(ctx, "sweeper")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Epoch != 1 {
		t.Errorf("Get epoch = %d, want 1", row.Epoch)
	}
	if row.HolderID != "host-a" {
		t.Errorf("Get holder_id = %q, want host-a", row.HolderID)
	}
}

// TestLeaderLock_ListScansEpoch: List must scan the epoch column
// into DaemonLeaderLock.Epoch for every returned row.
func TestLeaderLock_ListScansEpoch(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewLeaderLockRepository(db.DB)
	ctx := context.Background()

	now := time.Now().UTC()
	for _, wid := range []string{"sweeper", "autonomy"} {
		if _, _, err := repo.Acquire(ctx, wid, "host-a", now, 30*time.Second); err != nil {
			t.Fatalf("Acquire %s: %v", wid, err)
		}
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List len = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Epoch != 1 {
			t.Errorf("List row %q epoch = %d, want 1", r.WorkerID, r.Epoch)
		}
	}
}

// DeleteExpired is ONE statement whose facts are the audit's facts, and a row
// whose stored timestamps do not parse is NOT removed (leader-lock release
// contract 2026-09-25, review-20260925-8d83 F1). It used to SELECT, then DELETE,
// and assign time.Parse errors to _, so a malformed value audited a zero
// expiry in silence after the row was already gone.
func TestLeaderLock_DeleteExpiredKeepsARowItCannotRead(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewLeaderLockRepository(db.DB)
	ctx := context.Background()
	// "2000-garbage" sorts before any real timestamp, so the expiry predicate
	// matches; the parse is what must stop the delete.
	if _, err := db.ExecContext(ctx, `INSERT INTO daemon_leader_locks
		(worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch)
		VALUES ('w-bad', 'h1', '2000-garbage', '2000-garbage', '2000-garbage', 3)`); err != nil {
		t.Fatal(err)
	}
	row, err := repo.DeleteExpired(ctx, "w-bad", time.Now())
	if err == nil {
		t.Fatalf("an unparseable row must fail the release, got row=%+v", row)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_leader_locks WHERE worker_id = 'w-bad'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the row must survive a failed release: count=%d err=%v", n, err)
	}
}

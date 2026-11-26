//go:build integration

package postgres

// GitHub #861, scheduled-reminders design §11 R5/R6: a failed delivery
// attempt retries, re-arms with the count reset, or expires; only a firing
// row changes; the success writes reset the count.

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func attemptRow861(t *testing.T, ctx context.Context, db *DB, id string) (status string, errCount int, lastErr string, fireAt time.Time) {
	t.Helper()
	var le *string
	if err := db.QueryRowContext(ctx, `SELECT status, error_count, last_error, fire_at FROM dispatcher_reminders WHERE id = $1`, id).
		Scan(&status, &errCount, &le, &fireAt); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if le != nil {
		lastErr = *le
	}
	return status, errCount, lastErr, fireAt
}

func TestMarkAttemptFailed_861_ThreeOutcomesAndTheGuard(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	cleanup := func(id string) {
		t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, id) })
	}

	retryAt := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	r := firingTaskReminder(t, ctx, repo)
	cleanup(r.ID)
	if err := repo.MarkAttemptFailed(ctx, r.ID, "send failed", persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptRetry, At: retryAt}); err != nil {
		t.Fatal(err)
	}
	if st, n, le, at := attemptRow861(t, ctx, db, r.ID); st != "pending" || n != 1 || le != "send failed" || !at.Equal(retryAt) {
		t.Fatalf("retry: %s %d %q %v", st, n, le, at)
	}

	r2 := firingTaskReminder(t, ctx, repo)
	cleanup(r2.ID)
	_, _ = db.ExecContext(ctx, `UPDATE dispatcher_reminders SET error_count = 2 WHERE id = $1`, r2.ID)
	next := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	if err := repo.MarkAttemptFailed(ctx, r2.ID, "third failure", persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptRearm, At: next}); err != nil {
		t.Fatal(err)
	}
	if st, n, le, at := attemptRow861(t, ctx, db, r2.ID); st != "pending" || n != 0 || le != "third failure" || !at.Equal(next) {
		t.Fatalf("rearm: %s %d %q %v", st, n, le, at)
	}

	r3 := firingTaskReminder(t, ctx, repo)
	cleanup(r3.ID)
	if err := repo.MarkAttemptFailed(ctx, r3.ID, "gave up", persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptExpire}); err != nil {
		t.Fatal(err)
	}
	if st, n, le, _ := attemptRow861(t, ctx, db, r3.ID); st != "expired" || n != 1 || le != "gave up" {
		t.Fatalf("expire: %s %d %q", st, n, le)
	}

	// A row no longer firing (here, the expired one) does not change.
	err := repo.MarkAttemptFailed(ctx, r3.ID, "late", persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptRetry, At: retryAt})
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a non-firing row: err = %v, want ErrNotFound", err)
	}
	if st, _, le, _ := attemptRow861(t, ctx, db, r3.ID); st != "expired" || le != "gave up" {
		t.Fatalf("a non-firing row changed: %s %q", st, le)
	}
}

func TestReminderSuccessResetsTheAttemptCount_861(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewReminderRepository(db)
	ctx := context.Background()
	for _, fire := range []bool{true, false} {
		r := firingTaskReminder(t, ctx, repo)
		id := r.ID
		t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, id) })
		_, _ = db.ExecContext(ctx, `UPDATE dispatcher_reminders SET error_count = 2 WHERE id = $1`, id)
		var err error
		if fire {
			err = repo.MarkFired(ctx, id)
		} else {
			err = repo.Reschedule(ctx, id, time.Now().Add(time.Hour))
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, n, _, _ := attemptRow861(t, ctx, db, id); n != 0 {
			t.Fatalf("fire=%v: error_count = %d after success, want 0", fire, n)
		}
	}
}

package telegram

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #326, operator decision 2026-10-13 (05-scheduler §4.11 "Amended
// 2026-10-13"): /cancel may cancel a task parked in WAITING_FOR_CHILDREN,
// AWAITING_INPUT or AWAITING_EXTERNAL, as the UI and chat can;
// AWAITING_APPROVAL is still refused (it is answered with approve/reject),
// and finished tasks are refused as before. Over a real SQLite repository.
func TestHandleCancelCmd_ParkedStatuses_326(t *testing.T) {
	run := func(t *testing.T, st persistence.TaskStatus) (string, persistence.TaskStatus, *tgTeardownSpy) {
		t.Helper()
		db := sqlitetest.Memory(t)
		tasks := sqlite.NewTaskRepository(db.DB)
		ctx := context.Background()
		wf := "wf-326"
		now := time.Now().UTC()
		if err := tasks.Create(ctx, &persistence.Task{
			ID: "task-326", ProjectID: "p", WorkflowID: &wf, Status: st,
			Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		bot := makeBotWithTaskRepo(t, tasks)
		spy := &tgTeardownSpy{}
		WithTaskTeardown(spy)(bot)
		out := bot.handleCancelCmd(ctx, 0, []string{"/cancel", "task-326"})
		got, err := tasks.Get(ctx, "task-326")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return out, got.Status, spy
	}
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
	} {
		t.Run(string(st)+" is cancelled", func(t *testing.T) {
			out, row, spy := run(t, st)
			if row != persistence.TaskStatusCancelled || !strings.Contains(out, "Cancelled") {
				t.Fatalf("row = %s, reply = %q; want CANCELLED and a confirmation", row, out)
			}
			if !slices.Contains(spy.calls, "CancelChildren:task-326") {
				t.Errorf("teardown calls = %v; want the child cascade", spy.calls)
			}
		})
	}
	t.Run("AWAITING_APPROVAL is refused", func(t *testing.T) {
		out, row, spy := run(t, persistence.TaskStatusAwaitingApproval)
		if row != persistence.TaskStatusAwaitingApproval {
			t.Fatalf("row = %s; an approval must not be cancelled from /cancel", row)
		}
		if !strings.Contains(out, "AWAITING_APPROVAL") || !strings.Contains(out, "approval is answered") {
			t.Errorf("reply = %q; want the real status and why", out)
		}
		if len(spy.calls) != 0 {
			t.Errorf("teardown calls = %v; want none", spy.calls)
		}
	})
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed,
	} {
		t.Run(string(st)+" is refused as finished", func(t *testing.T) {
			out, row, _ := run(t, st)
			if row != st || !strings.Contains(out, "already "+string(st)) {
				t.Errorf("row = %s, reply = %q; want %s unchanged and 'already %s'", row, out, st, st)
			}
		})
	}
}

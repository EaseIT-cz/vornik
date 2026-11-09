package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #490 (05-scheduler §4.11 "Amended 2026-11-07"): an approval the
// cancel cascade closed stays in the inbox for 24 hours, saying which task
// above it was cancelled and why. Before, the row vanished with no trace.
func TestInbox_490_ApprovalClosedRow(t *testing.T) {
	now := time.Now()
	reason := persistence.ApprovalClosedPrefix + "task t-parent above it was cancelled: approval timed out"
	closed := &persistence.Task{ID: "t-closed", ProjectID: "p1", Status: persistence.TaskStatusCancelled,
		LastError: &reason, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)}
	var since time.Time
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(nil, f), nil
		},
		ListApprovalsClosedSinceFunc: func(_ context.Context, _ []string, s time.Time) ([]*persistence.Task, error) {
			since = s
			return []*persistence.Task{closed}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"t-closed", "Approval closed", "task t-parent above it was cancelled: approval timed out", "/ui/tasks/t-closed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the closed-approval row lacks %q:\n%s", want, body)
		}
	}
	if d := now.Add(-24 * time.Hour).Sub(since); d < -time.Minute || d > time.Minute {
		t.Fatalf("the query window starts %v, want 24 hours back", since)
	}
	// Informational: it does not raise the "needs you" count.
	if n := countActionableItems([]inboxItem{newApprovalClosedItem(closed)}); n != 0 {
		t.Fatalf("a closed approval counted %d toward the badge, want 0", n)
	}
}

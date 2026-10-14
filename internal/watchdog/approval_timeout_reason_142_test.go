package watchdog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #142 §S3 #17 (outcome-quality LLD §S5 #17). The
// approval-timeout sweep cancelled a parked task and left no reason on it:
// the row read CANCELLED with no last_error, exactly like an operator's
// cancel, and the reason lived only in an info log line.
func TestSweepExpiredApprovals_RecordsWhyAndWhatToDo(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	parked := &persistence.Task{ID: "parked", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)}
	tasks.setTask(parked)
	tasks.listResult = []*persistence.Task{parked}
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())

	tasks.mu.Lock()
	got := tasks.tasks["parked"]
	tasks.mu.Unlock()
	if got.Status != persistence.TaskStatusCancelled {
		t.Fatalf("status = %s, want CANCELLED", got.Status)
	}
	if got.LastError == nil {
		t.Fatal("the cancelled task carries no last_error: it reads like an operator's cancel")
	}
	for _, want := range []string{"approval timed out", "96h", "Re-submit", "autonomy.approval_timeout_hours"} {
		if !strings.Contains(*got.LastError, want) {
			t.Errorf("last_error %q lacks %q", *got.LastError, want)
		}
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassCancelled {
		t.Fatalf("last_error_class = %v, want %s", got.LastErrorClass, persistence.TaskFailureClassCancelled)
	}
	// Review ace9 #3: the class must survive the live-event normaliser
	// rather than be published as UNKNOWN.
	if c := livepubsub.ClosedTaskClass(*got.LastErrorClass); c != persistence.TaskFailureClassCancelled {
		t.Errorf("ClosedTaskClass(%q) = %q: the reason's class is erased downstream", *got.LastErrorClass, c)
	}
}

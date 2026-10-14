package dispatcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #142 §S3 #7 (outcome-quality LLD §S5 #7).

// ctxStrictAuditRepo refuses a write on a finished context, as both database
// drivers do.
type ctxStrictAuditRepo struct{ stubAuditRepo }

func (s *ctxStrictAuditRepo) Log(ctx context.Context, e *persistence.ToolAuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.stubAuditRepo.Log(ctx, e)
}

// The audit row of the tool call that used up the turn is the one an operator
// most needs, and it was written on the turn context that had just ended.
func TestLogAudit_WritesAfterTheTurnContextEnded(t *testing.T) {
	audit := &ctxStrictAuditRepo{}
	te := newExecutor(withAuditRepo(audit))
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	te.logAudit(ctx, "wait_for_task", "{}", "stopped", "p", time.Minute)
	if len(audit.entries) != 1 {
		t.Fatalf("audit rows = %d, want 1: the row was refused on the expired turn context", len(audit.entries))
	}
}

// wait_for_task's 600s default outlived the 5-minute UI and Telegram turns: the
// wait ran into the turn's deadline and the model never got the "still running"
// result. The wait now stops a reserve before the turn's deadline. Here the
// turn has less than the reserve left, so the tool reads once and returns.
func TestWaitForTask_StopsBeforeTheTurnDeadline(t *testing.T) {
	repo := newPollingRepo(&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusRunning})
	te := &ToolExecutor{taskRepo: repo}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := te.waitForTask(ctx, `{"task_id":"task_x"}`, []string{"p"})
	if ctx.Err() != nil {
		t.Fatalf("the wait ran into the turn's own deadline; result %q came too late for the turn to use", res.Content)
	}
	for _, want := range []string{"chat turn", "RUNNING", "get_task_status", "Do not wait"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result %q lacks %q", res.Content, want)
		}
	}
}

func TestWaitForTask_ExpiredTurnIsATimeLimitNotACancel(t *testing.T) {
	repo := newPollingRepo(&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusRunning})
	te := &ToolExecutor{taskRepo: repo}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	res := te.waitForTask(ctx, `{"task_id":"task_x"}`, []string{"p"})
	if strings.Contains(res.Content, "cancelled") || !strings.Contains(res.Content, "time limit") {
		t.Errorf("result %q: a turn deadline must read as the turn's time limit, not a cancel", res.Content)
	}
}

func TestWaitForTask_CancelStaysACancel(t *testing.T) {
	repo := newPollingRepo(&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusRunning})
	te := &ToolExecutor{taskRepo: repo}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := te.waitForTask(ctx, `{"task_id":"task_x"}`, []string{"p"})
	if !strings.Contains(res.Content, "cancelled") {
		t.Errorf("result %q: a cancel must still read as cancelled", res.Content)
	}
}

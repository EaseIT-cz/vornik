package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// staleGetTaskRepo serves a stale snapshot from Get while writes go to the
// real in-memory row — the read-then-write race made deterministic.
type staleGetTaskRepo struct {
	*MockTaskRepo
	stale persistence.TaskStatus
}

func (r *staleGetTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	t, err := r.MockTaskRepo.Get(ctx, id)
	if err != nil || t == nil {
		return t, err
	}
	t.Status = r.stale
	return t, nil
}

// Scheduler design §4.10: the CPC cancel-on-timeout cascade read the callee's
// status, then wrote CANCELLED with the bare UpdateStatus, so a callee that
// COMPLETED between the two was recorded as cancelled.
func TestCPCCascadeCancel_DoesNotOverwriteACalleeThatCompletedMeanwhile(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	id := "callee-raced"
	tr.AddTask(&persistence.Task{ID: id, Status: persistence.TaskStatusCompleted})
	e.taskRepo = &staleGetTaskRepo{MockTaskRepo: tr, stale: persistence.TaskStatusRunning}
	NewCPCTimeoutScanner(e).cascadeCancelCallee(context.Background(),
		&persistence.CrossProjectCall{ID: "c1", CalleeTaskID: &id})
	got, _ := tr.Get(context.Background(), id)
	if got.Status != persistence.TaskStatusCompleted {
		t.Fatalf("callee status %s — a COMPLETED callee was overwritten by the timeout cascade", got.Status)
	}
}

func TestCPCCascadeCancel_CancelsALiveCallee(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	id := "callee-live"
	tr.AddTask(&persistence.Task{ID: id, Status: persistence.TaskStatusRunning})
	NewCPCTimeoutScanner(e).cascadeCancelCallee(context.Background(),
		&persistence.CrossProjectCall{ID: "c1", CalleeTaskID: &id})
	got, _ := tr.Get(context.Background(), id)
	if got.Status != persistence.TaskStatusCancelled {
		t.Fatalf("live callee status %s, want CANCELLED", got.Status)
	}
}

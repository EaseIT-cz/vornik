package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// hangingExecRepo blocks GetByTaskID until its context ends, as a database
// that has stopped answering would. With hang unset it answers like
// production, a miss included (the repository miss contract).
type hangingExecRepo struct {
	*MockExecRepo
	hang        bool
	sawDeadline bool
}

func (h *hangingExecRepo) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	if !h.hang {
		return h.MockExecRepo.GetByTaskID(ctx, taskID)
	}
	_, h.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHangingExecRepo_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "ExecutionRepository.GetByTaskID", func() (*persistence.Execution, error) {
		return (&hangingExecRepo{MockExecRepo: NewMockExecRepo()}).GetByTaskID(context.Background(), "task-absent")
	})
}

// TestResume_DatabaseCallsAreBoundedWhileHoldingTheLock — audit 2026-10-12,
// GitHub #142 §S7 #9: Resume holds e.mu across its database calls, which ran
// on context.Background() with no bound, so one database call that never
// answered held e.mu — and with it every Execute, Pause and Cancel — forever.
func TestResume_DatabaseCallsAreBoundedWhileHoldingTheLock(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	hang := &hangingExecRepo{MockExecRepo: er, hang: true}
	e.execRepo = hang
	e.resumeIOTimeout = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := e.Resume("t1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Resume error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "did not answer within 20ms") {
			t.Errorf("Resume error %q does not name the bound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume is still blocked on a database call with no deadline, holding e.mu")
	}
	if !hang.sawDeadline {
		t.Error("GetByTaskID ran with no deadline")
	}
	// e.mu must be free again.
	locked := make(chan struct{})
	go func() {
		e.mu.Lock()
		n := len(e.activeExecutions)
		e.mu.Unlock()
		_ = n
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("e.mu is still held after Resume returned")
	}
}

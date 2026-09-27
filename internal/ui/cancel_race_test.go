package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Scheduler design §4.10. The UI cancel paths read a status, then wrote
// CANCELLED with the bare UpdateStatus, so a task that COMPLETED between the
// two was recorded as cancelled. The window cannot be hit on demand: Get
// returns the stale snapshot while the stored row has already moved on.
func uiStaleRepo(stale, live persistence.TaskStatus) (*mocks.MockTaskRepository, *persistence.TaskStatus) {
	stored := live
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, stale), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, s persistence.TaskStatus) error {
			stored = s
			return nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			for _, f := range from {
				if f == stored {
					stored = to
					return true, nil
				}
			}
			return false, nil
		},
	}, &stored
}

func TestCancelOne_DoesNotOverwriteATaskThatCompletedMeanwhile(t *testing.T) {
	repo, stored := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusCompleted)
	srv := NewServer(WithTaskRepository(repo))
	got := srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1")
	if *stored != persistence.TaskStatusCompleted {
		t.Fatalf("stored %s — a COMPLETED task was overwritten by a UI cancel", *stored)
	}
	if got {
		t.Fatal("cancelOne reported a cancel that did not happen")
	}
}

// The state machine's rule is "any non-terminal → CANCELLED"; the UI's list
// had omitted AWAITING_APPROVAL.
func TestCancelOne_CancelsAnAwaitingApprovalTask(t *testing.T) {
	repo, stored := uiStaleRepo(persistence.TaskStatusAwaitingApproval, persistence.TaskStatusAwaitingApproval)
	srv := NewServer(WithTaskRepository(repo))
	if !srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1") ||
		*stored != persistence.TaskStatusCancelled {
		t.Fatalf("AWAITING_APPROVAL not cancellable from the UI: stored=%s", *stored)
	}
}

func TestCancelExecutionOne_DoesNotOverwriteATaskThatCompletedMeanwhile(t *testing.T) {
	repo, stored := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusCompleted)
	execMarked := false
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.ExecutionStatus) error {
			execMarked = true
			return nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(repo))
	got := srv.cancelExecutionOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "e1")
	if *stored != persistence.TaskStatusCompleted {
		t.Fatalf("stored %s — a COMPLETED task was overwritten by an execution cancel", *stored)
	}
	if got || execMarked {
		t.Fatalf("a cancel that did not happen was reported (%v) or marked the execution (%v)", got, execMarked)
	}
}

// teardownSpy records CancelIfActive on top of closeNotifySpy's
// NotifyChildTerminal / CancelChildren recording.
type teardownSpy struct {
	closeNotifySpy
	teardowns []string
}

func (s *teardownSpy) CancelIfActive(taskID string) (bool, error) {
	s.teardowns = append(s.teardowns, taskID)
	return true, nil
}

// Review a3d9 F2: a successful execution cancel tears down through the
// executor's live map and then marks the execution row.
func TestCancelExecutionOne_ALiveCancelTearsDownThenMarksTheExecution(t *testing.T) {
	repo, stored := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusRunning)
	var order []string
	spy := &teardownSpy{}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, st persistence.ExecutionStatus) error {
			order = append(order, "exec:"+string(st)+fmt.Sprintf(":after-%d-teardowns", len(spy.teardowns)))
			return nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(repo), WithExecutor(spy))
	if !srv.cancelExecutionOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "e1") {
		t.Fatal("a live execution cancel was refused")
	}
	if *stored != persistence.TaskStatusCancelled {
		t.Fatalf("task stored %s, want CANCELLED", *stored)
	}
	if len(spy.teardowns) != 1 || spy.teardowns[0] != "t1" {
		t.Fatalf("teardown not asked of the executor: %v", spy.teardowns)
	}
	if len(order) != 1 || order[0] != "exec:"+string(persistence.ExecutionStatusCancelled)+":after-1-teardowns" {
		t.Fatalf("execution row must be marked CANCELLED after the teardown request: %v", order)
	}
}

// Review a3d9 F3: when the conditional write is refused, none of cancelOne's
// four post-write effects fire.
func TestCancelOne_ARefusedCancelFiresNoSideEffects(t *testing.T) {
	repo, _ := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusCompleted)
	parent := "parent-1"
	inner := repo.GetFunc
	repo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		tk, err := inner(ctx, id)
		if tk != nil {
			tk.ParentTaskID = &parent
		}
		return tk, err
	}
	spy := &teardownSpy{}
	srv := NewServer(WithTaskRepository(repo), WithExecutor(spy))
	if srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1") {
		t.Fatal("a refused cancel reported success")
	}
	if len(spy.teardowns) != 0 || len(spy.calls) != 0 || len(spy.cascadeCalls) != 0 {
		t.Fatalf("side effects fired for a cancel that did not happen: teardown=%v unblock=%v cascade=%v",
			spy.teardowns, spy.calls, spy.cascadeCalls)
	}
}

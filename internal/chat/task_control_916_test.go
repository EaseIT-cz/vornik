package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #916 item 2 (Telegram task-control design, Amendment 2026-11-25):
// the chat model's retry_task read the task, accepted only FAILED, reset
// attempt to 0 and wrote the row back, racing the scheduler. It now uses
// the atomic RequeueTerminalTask that /retry and the inbox use.
func TestRetryTask_UsesAtomicRequeue_916(t *testing.T) {
	var requeued []string
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			st := persistence.TaskStatusCancelled
			if id == "running" {
				st = persistence.TaskStatusRunning
			}
			return &persistence.Task{ID: id, Status: st, MaxAttempts: 3}, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, id string, _, _ int) (bool, error) {
			requeued = append(requeued, id)
			return id != "running", nil
		},
		UpdateFunc: func(context.Context, *persistence.Task) error {
			t.Fatal("retry_task must not write the row back with Update")
			return nil
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionRetryTask, TaskID: "cancelled", Confirm: true}, repo, nil, nil)
	if err != nil || !res.Success {
		t.Fatalf("a CANCELLED task must be retried: %v %q", err, res.Message)
	}
	res, err = ExecuteAction(context.Background(), Action{Type: ActionRetryTask, TaskID: "running", Confirm: true}, repo, nil, nil)
	if err == nil || res.Success || !strings.Contains(res.Message, "RUNNING") {
		t.Fatalf("a RUNNING task must be refused with its status: %v %q", err, res.Message)
	}
	if len(requeued) != 2 {
		t.Fatalf("RequeueTerminalTask calls = %v", requeued)
	}
}

// #916 item 4: the chat model's cancel_task may not cancel a task awaiting
// approval (#326: an approval is answered, not cancelled).
func TestCancelTask_RefusesAwaitingApproval_916(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			t.Fatal("an awaiting-approval task must not reach the cancel write")
			return false, nil
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionCancelTask, TaskID: "t1", Confirm: true}, repo, nil, nil)
	if err == nil || res.Success {
		t.Fatalf("cancelled a task awaiting approval: %q", res.Message)
	}
}

// #916 item 3: no task-control reply quotes a store error.
func TestTaskControl_StoreErrorIsNotQuoted_916(t *testing.T) {
	const secret = "pq: password authentication failed for host db.internal"
	failGet := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return nil, errors.New(secret) },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New(secret)
		},
	}
	for _, a := range []Action{
		{Type: ActionListTasks, Project: "p"},
		{Type: ActionGetStatus, TaskID: "t1"},
		{Type: ActionCancelTask, TaskID: "t1", Confirm: true},
		{Type: ActionRetryTask, TaskID: "t1", Confirm: true},
	} {
		res, _ := ExecuteAction(context.Background(), a, failGet, nil, nil)
		if strings.Contains(res.Message, "password") || strings.Contains(res.Message, "db.internal") {
			t.Errorf("%s quotes the store error: %q", a.Type, res.Message)
		}
	}
}

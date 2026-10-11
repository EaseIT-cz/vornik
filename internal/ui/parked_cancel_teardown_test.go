package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #304 (2026-10-08): the UI's parked cancel sites (05-scheduler
// §4.11, #14 reject, #15 budget abandon, #16 taint cancel) moved the row to
// CANCELLED and did nothing after it, so a parked child left its delegating
// parent in WAITING_FOR_CHILDREN until the watchdog's stranded-parent sweep
// and left its own children running. Each now goes through taskcancel.Cancel.

func uiCancellingRepo() *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return to == persistence.TaskStatusCancelled, nil
		},
	}
}

func assertUIParkedTeardown(t *testing.T, spy *closeNotifySpy, id string) {
	t.Helper()
	if len(spy.calls) != 1 || spy.calls[0] != id {
		t.Errorf("NotifyChildTerminal calls = %v, want [%s]: the parent must be unblocked", spy.calls, id)
	}
	if len(spy.cascadeCalls) != 1 || spy.cascadeCalls[0] != id {
		t.Errorf("CancelChildren calls = %v, want [%s]: the children must be cancelled", spy.cascadeCalls, id)
	}
}

func uiAnswer(t *testing.T, srv *Server, task *persistence.Task, choice string) string {
	t.Helper()
	form := url.Values{"checkpoint_id": []string{*task.OpenCheckpointID}, "choice": []string{choice}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return srv.uiAnswerCheckpoint(context.Background(), task, req)
}

func TestUIRejectTask_NotifiesParent(t *testing.T) {
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(uiCancellingRepo()), WithTaskMessageRepository(&uiTcStubMsgRepo{}), WithExecutor(spy))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingApproval}
	if got := srv.uiRejectTask(context.Background(), task); got != "rejected" {
		t.Fatalf("uiRejectTask = %q, want rejected", got)
	}
	assertUIParkedTeardown(t, spy, "t1")
}

func TestUIBudgetAbandon_NotifiesParent(t *testing.T) {
	open := "cp1"
	meta, _ := json.Marshal(map[string]any{
		"kind":     "decision",
		"decision": map[string]any{"kind": "budget", "spent_usd": 12.0, "budget_usd": 10.0},
		"options":  []map[string]any{{"id": "increase", "label": "Increase"}, {"id": "abandon", "label": "Abandon"}},
	})
	msgRepo := &uiTcStubMsgRepo{checkpoint: &persistence.TaskMessage{ID: open, MessageKind: persistence.TaskMessageKindCheckpoint, Metadata: meta}}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(uiCancellingRepo()), WithTaskMessageRepository(msgRepo), WithExecutor(spy))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open}
	if got := uiAnswer(t, srv, task, "abandon"); got != "task-cancelled" {
		t.Fatalf("got %q, want task-cancelled", got)
	}
	assertUIParkedTeardown(t, spy, "t1")
}

func TestUITaintCancel_NotifiesParent(t *testing.T) {
	open := "cp1"
	msgRepo := &uiTcStubMsgRepo{checkpoint: uiTaintCheckpointMsg(open, "abc")}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(uiCancellingRepo()), WithTaskMessageRepository(msgRepo), WithExecutor(spy))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open}
	if got := uiAnswer(t, srv, task, "cancel"); got != "task-cancelled" {
		t.Fatalf("got %q, want task-cancelled", got)
	}
	assertUIParkedTeardown(t, spy, "t1")
}

// A failed cancel write is reported as failed, not as a stale row
// (review-20261009-7123 F3), and tears nothing down.
func TestUIRejectTask_WriteErrorIsFailedNotStale(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			return false, errors.New("db down")
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}), WithExecutor(spy))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingApproval}
	if got := srv.uiRejectTask(context.Background(), task); got != "rejected-failed" {
		t.Fatalf("uiRejectTask = %q, want rejected-failed", got)
	}
	if len(spy.calls)+len(spy.cascadeCalls) != 0 {
		t.Errorf("a failed write ran teardown: %v %v", spy.calls, spy.cascadeCalls)
	}
}

// The shared flip helper still serves verbs that do not cancel: approve
// goes on to QUEUED with no cancel teardown.
func TestUIApproveTask_NoCancelTeardown(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}), WithExecutor(spy))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingApproval}
	if got := srv.uiApproveTask(context.Background(), task); got != "approved" {
		t.Fatalf("uiApproveTask = %q, want approved", got)
	}
	if len(spy.calls)+len(spy.cascadeCalls) != 0 {
		t.Errorf("approve ran cancel teardown: %v %v", spy.calls, spy.cascadeCalls)
	}
}

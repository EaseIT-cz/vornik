package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #304 (2026-10-08): the parked cancel sites (05-scheduler §4.11,
// sites #17, #18 and the REST reject #20) moved the row to CANCELLED and did
// nothing after it, so a parked child that was abandoned, blocked or
// rejected left its delegating parent in WAITING_FOR_CHILDREN until the
// watchdog's stranded-parent sweep, and left its own children running.
// Each now goes through taskcancel.Cancel: after the write, the parent
// unblock and the child cascade.

func cancelledTransition(t *testing.T) func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
	t.Helper()
	return func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		require.Equal(t, persistence.TaskStatusCancelled, to)
		return true, nil
	}
}

func assertParkedTeardown(t *testing.T, spy *cancelTeardownSpy, id string) {
	t.Helper()
	assert.Contains(t, spy.calls, "NotifyChildTerminal:"+id, "the parent of a cancelled parked task must be unblocked")
	assert.Contains(t, spy.calls, "CancelChildren:"+id, "the children of a cancelled parked task must be cancelled")
}

func TestAPIBudgetAbandon_NotifiesParent(t *testing.T) {
	cp := "cp1"
	taskRepo := budgetTaskRepo(cp, nil, cancelledTransition(t))
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	spy := &cancelTeardownSpy{}
	srv.executor = spy
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"abandon","content":"stop"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertParkedTeardown(t, spy, tcTaskID)
}

func TestAPITaintCancel_NotifiesParent(t *testing.T) {
	cp := "cp1"
	taskRepo := taintTaskRepo(cp, cancelledTransition(t))
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) {
		return taintCheckpointMsg(cp, "abc"), nil
	}}
	srv := tcServer(taskRepo, msgRepo)
	spy := &cancelTeardownSpy{}
	srv.executor = spy
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"cancel","content":"block it"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertParkedTeardown(t, spy, tcTaskID)
}

func TestAPIRejectTask_NotifiesParent(t *testing.T) {
	const projectID, taskID = "test-proj", "task-awaiting-approval-3"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: projectID, Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: cancelledTransition(t),
	}
	spy := &cancelTeardownSpy{}
	s := buildPauseResumeServer(taskRepo, &stubTaskMessageRepo{}, spy)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/reject", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.RejectTask(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertParkedTeardown(t, spy, taskID)
}

// A refused write runs none of the post-write steps.
func TestAPIBudgetAbandon_RefusedWriteTearsNothingDown(t *testing.T) {
	cp := "cp1"
	taskRepo := budgetTaskRepo(cp, nil, func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
		return false, nil
	})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	spy := &cancelTeardownSpy{}
	srv.executor = spy
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"abandon","content":"stop"}`))
	srv.AnswerCheckpoint(httptest.NewRecorder(), req)
	assert.Empty(t, spy.calls)
}

package executor

// GitHub #636 and #664 (inter-project orchestration design, amendment
// 2026-11-06). Found at review: a call_project caller resumed at its
// on_success step whatever its cross-project call resolved to, so a rejected
// or timed-out call carried on down the success path, and a completed call's
// answer was unreadable through ${outputs}. The caller's resume now reads the
// CPC first. These drive executeWorkflowAttempt from the checkpoint a
// call_project step leaves (next step = on_success, CallResume set) with the
// CPC in each state.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// captureHandler records the PrevResult each system step received.
type captureHandler struct {
	name string
	mu   *sync.Mutex
	got  map[string]json.RawMessage
}

func (h *captureHandler) Name() string { return h.name }
func (h *captureHandler) Execute(_ context.Context, in SystemStepInput) (SystemStepResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got[in.StepID] = append(json.RawMessage(nil), in.PrevResult...)
	return SystemStepResult{Result: json.RawMessage(`{"ok":true}`)}, nil
}

type callResumeRun struct {
	err   error
	ran   map[string]json.RawMessage
	state executionState
}

func callResumePlan(withOnFail bool) *executionPlan {
	call := registry.WorkflowStep{Type: "call_project", TargetProject: "p2", TargetWorkflow: "w", OnSuccess: "reader"}
	if withOnFail {
		call.OnFail = "handle"
	}
	return &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID: "wf", Entrypoint: "call", MaxIterations: 10, MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"call":   call,
				"reader": {Type: "system", Handler: "capture", OnSuccess: "done"},
				"handle": {Type: "system", Handler: "capture", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"done":   {Status: "COMPLETED"},
				"failed": {Status: "FAILED", Message: "workflow failed"},
			},
		},
	}
}

// runCallResume resumes an execution paused by the "call" step. cpc nil
// means the row is absent; withResume false is a checkpoint written before
// the upgrade (no CallResume).
func runCallResume(t *testing.T, cpc *persistence.CrossProjectCall, withOnFail, withResume bool, repo persistence.CrossProjectCallRepository) callResumeRun {
	t.Helper()
	ran := map[string]json.RawMessage{}
	reg := NewSystemHandlerRegistry()
	reg.Register(&captureHandler{name: "capture", mu: &sync.Mutex{}, got: ran})
	er := NewMockExecRepo()
	e := NewWithOptions(NewMockRuntime(), er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	if repo == nil {
		m := newMockCPCRepo()
		if cpc != nil {
			m.rows[cpc.ID] = cpc
		}
		repo = m
	}
	e.cpcRepo = repo

	st := executionState{
		CurrentStepID:  "reader",
		CompletedSteps: []string{"call"},
		LastResult:     json.RawMessage(`{"cross_project_call_id":"ccp_1","callee_task_id":"task_callee"}`),
	}
	if withResume {
		st.CallResume = &callResume{StepID: "call", CPCID: "ccp_1"}
	}
	snap, err := json.Marshal(st)
	require.NoError(t, err)
	next := "reader"
	exec := &persistence.Execution{ID: "x-call", TaskID: "t", ProjectID: "p", CurrentStepID: &next,
		CompletedSteps: []string{"call"}, StateSnapshot: snap}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, runErr := e.executeWorkflowAttempt(context.Background(), task, exec, callResumePlan(withOnFail), time.Minute)
	got, _ := er.Get(context.Background(), "x-call")
	return callResumeRun{err: runErr, ran: ran, state: loadExecutionState(got)}
}

func cpcRow(status persistence.CrossProjectCallStatus, envelope string, msg string) *persistence.CrossProjectCall {
	c := &persistence.CrossProjectCall{ID: "ccp_1", CallerTaskID: "t", CallerStepID: "call", Status: status, CreatedAt: time.Now()}
	if envelope != "" {
		c.ResultEnvelope = []byte(envelope)
	}
	if msg != "" {
		c.ErrorMessage = &msg
	}
	return c
}

func TestCallResume_CompletedHandsTheEnvelopeDataOn_636(t *testing.T) {
	r := runCallResume(t, cpcRow(persistence.CPCStatusCompleted, `{"schema":"x.v1","data":{"answer":"42"}}`, ""), true, true, nil)
	require.NoError(t, r.err)
	require.Contains(t, r.ran, "reader")
	assert.NotContains(t, r.ran, "handle")
	assert.JSONEq(t, `{"answer":"42"}`, string(r.ran["reader"]), "the next step receives the envelope's data")
	assert.JSONEq(t, `{"answer":"42"}`, string(r.state.StepResults["call"]), "${outputs.call.*} reads the envelope's data")
	assert.Nil(t, r.state.CallResume, "CallResume is cleared once applied")
}

func TestCallResume_BadOutcomesTakeOnFail_664(t *testing.T) {
	for _, st := range []persistence.CrossProjectCallStatus{persistence.CPCStatusRejected, persistence.CPCStatusFailed, persistence.CPCStatusTimedOut} {
		t.Run(string(st), func(t *testing.T) {
			r := runCallResume(t, cpcRow(st, "", "callee said no"), true, true, nil)
			require.NoError(t, r.err)
			assert.NotContains(t, r.ran, "reader", "a %s call must not run the success path", st)
			require.Contains(t, r.ran, "handle", "a %s call takes on_fail", st)
			assert.Contains(t, string(r.ran["handle"]), "ccp_1")
			assert.Contains(t, string(r.ran["handle"]), string(st))
		})
	}
}

func TestCallResume_BadOutcomeWithoutOnFailFailsTheTask_664(t *testing.T) {
	for _, st := range []persistence.CrossProjectCallStatus{persistence.CPCStatusRejected, persistence.CPCStatusFailed, persistence.CPCStatusTimedOut} {
		t.Run(string(st), func(t *testing.T) {
			r := runCallResume(t, cpcRow(st, "", "callee said no"), false, true, nil)
			require.Error(t, r.err)
			assert.Empty(t, r.ran, "nothing runs after a %s call with no on_fail", st)
			var classed interface{ FailureClass() string }
			require.ErrorAs(t, r.err, &classed)
			assert.Equal(t, persistence.TaskFailureClassCrossProjectCallFailed, classed.FailureClass())
			assert.Contains(t, r.err.Error(), string(st))
		})
	}
}

func TestCallResume_PendingOrRunningRePauses_664(t *testing.T) {
	for _, st := range []persistence.CrossProjectCallStatus{persistence.CPCStatusPending, persistence.CPCStatusRunning} {
		t.Run(string(st), func(t *testing.T) {
			r := runCallResume(t, cpcRow(st, "", ""), true, true, nil)
			require.ErrorIs(t, r.err, errExecutionPaused)
			assert.Empty(t, r.ran, "a %s call runs neither path", st)
		})
	}
}

func TestCallResume_UnreadableCPCFailsTheTask_664(t *testing.T) {
	broken := newMockCPCRepo()
	broken.getErr = errors.New("db down")
	r := runCallResume(t, nil, true, true, broken)
	require.Error(t, r.err)
	assert.Empty(t, r.ran, "an unreadable CPC never resumes as success")
	var classed interface{ FailureClass() string }
	require.ErrorAs(t, r.err, &classed)
	assert.Equal(t, persistence.TaskFailureClassCrossProjectCallFailed, classed.FailureClass())
}

// A checkpoint written before the upgrade has no CallResume: it resumes as
// it always did, at on_success, without reading the CPC.
func TestCallResume_OldCheckpointResumesUnchanged_664(t *testing.T) {
	r := runCallResume(t, cpcRow(persistence.CPCStatusRejected, "", "no"), true, false, nil)
	require.NoError(t, r.err)
	assert.Contains(t, r.ran, "reader")
	assert.NotContains(t, r.ran, "handle")
}

// Dispatch records CallResume beside the bookkeeping LastResult.
func TestCallResume_DispatchRecordsCallResume_636(t *testing.T) {
	st := executionState{}
	recordCallResume(&st, "call", callProjectResult{CPCId: "ccp_9", CalleeTaskID: "task_c"})
	require.NotNil(t, st.CallResume)
	assert.Equal(t, "call", st.CallResume.StepID)
	assert.Equal(t, "ccp_9", st.CallResume.CPCID)
}

// Rule 3: a FAILED callee (CrossProjectCallID set) whose parent paused with
// CallResume re-queues the parent, so the resume routes on the failed CPC
// (on_fail) instead of the delegation bubble-up. A failed in-project child,
// or a parent paused before the upgrade, still bubbles up.
func unblockAfterFailedChild(t *testing.T, crossProject, parentAwaitsCall bool) *persistence.Task {
	t.Helper()
	parentID := "parent-cpc"
	parent := &persistence.Task{ID: parentID, Status: persistence.TaskStatusWaitingForChildren, Attempt: 3, MaxAttempts: 3}
	child := &persistence.Task{ID: "callee", ParentTaskID: &parentID, Status: persistence.TaskStatusFailed}
	if crossProject {
		cpc := "ccp_1"
		child.CrossProjectCallID = &cpc
	}
	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc:                   func(context.Context, string) (*persistence.Task, error) { return parent, nil },
		GetChildrenFunc:           func(context.Context, string) ([]*persistence.Task, error) { return []*persistence.Task{child}, nil },
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	er := NewMockExecRepo()
	st := executionState{}
	if parentAwaitsCall {
		st.CallResume = &callResume{StepID: "call", CPCID: "ccp_1"}
	}
	snap, _ := json.Marshal(st)
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{ID: "x-parent", TaskID: parentID, ProjectID: "p", StateSnapshot: snap}))
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo, execRepo: er}
	e.checkParentUnblock(context.Background(), child)
	return parent
}

func TestCheckParentUnblock_FailedCalleeRequeuesTheCaller_664(t *testing.T) {
	assert.Equal(t, persistence.TaskStatusQueued, unblockAfterFailedChild(t, true, true).Status,
		"the caller resumes, so its resume takes on_fail")
}

func TestCheckParentUnblock_FailedInProjectChildStillBubblesUp_664(t *testing.T) {
	assert.Equal(t, persistence.TaskStatusFailed, unblockAfterFailedChild(t, false, true).Status)
}

func TestCheckParentUnblock_OldCheckpointStillBubblesUp_664(t *testing.T) {
	assert.Equal(t, persistence.TaskStatusFailed, unblockAfterFailedChild(t, true, false).Status,
		"a parent paused before the upgrade keeps the old bubble-up")
}

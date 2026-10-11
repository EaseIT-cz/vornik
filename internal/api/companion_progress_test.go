package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// GitHub #119 item 9 (2026-10-08): progress while a task runs, closed fields
// only (live-task-observation design, "Change 2026-10-08: progress while a
// task runs"). Before this change status and a non-terminal result answered
// identical bytes at every point of a run (C0, P0.1).

// threeStepWorkflow is read -> curate -> write, with an on_fail branch to
// repair that is not on the on_success chain.
func threeStepWorkflow(id string) *registry.Workflow {
	return &registry.Workflow{
		ID:         id,
		Entrypoint: "read",
		Steps: map[string]registry.WorkflowStep{
			"read":   {Type: "agent", Role: "reader", OnSuccess: "curate"},
			"curate": {Type: "agent", Role: "worker", OnSuccess: "write", OnFail: "repair"},
			"write":  {Type: "agent", Role: "worker", OnSuccess: "done"},
			"repair": {Type: "agent", Role: "worker", OnSuccess: "write"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
}

type progressFixture struct {
	srv  *Server
	raw  string
	pub  livepubsub.Publisher
	task *persistence.Task
	exec *persistence.Execution
}

// newProgressFixture serves one running task whose newest execution runs the
// given step of the three-step workflow, pinned by its snapshot.
func newProgressFixture(t *testing.T, broker bool, step string) *progressFixture {
	t.Helper()
	var (
		srv     *Server
		keyRepo *memAPIKeyRepo
		taskRep *mocks.MockTaskRepository
		project = "alpha"
	)
	if broker {
		srv, keyRepo, taskRep = newBrokerMCPServer(t)
		project = "broker-acme"
	} else {
		srv, keyRepo, taskRep = newCompanionMCPServer(t)
	}
	f := &progressFixture{srv: srv}
	f.raw, _ = seedCompanionKey(t, keyRepo, project, nil)
	wfID := "three-step"
	wf := threeStepWorkflow(wfID)
	snap, err := json.Marshal(wf)
	require.NoError(t, err)
	f.task = &persistence.Task{ID: "task_p", ProjectID: project, WorkflowID: &wfID, Status: persistence.TaskStatusRunning, Attempt: 1}
	f.exec = &persistence.Execution{ID: "exec_p", TaskID: "task_p", ProjectID: project, WorkflowID: wfID,
		WorkflowRevision: wf.Hash(), WorkflowSnapshot: snap, Status: persistence.ExecutionStatusRunning, CurrentStepID: strp(step)}
	taskRep.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		if id != f.task.ID {
			return nil, persistence.ErrNotFound
		}
		cp := *f.task
		return &cp, nil
	}
	srv.executionRepo = &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id != f.task.ID || f.exec == nil {
				return nil, persistence.ErrNotFound
			}
			cp := *f.exec
			return &cp, nil
		},
	}
	f.pub = livepubsub.New(50)
	srv.liveSub = f.pub
	return f
}

func (f *progressFixture) call(t *testing.T, tool string, args map[string]any) map[string]any {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["task_id"] = f.task.ID
	text, isErr := callTool(t, f.srv, f.raw, tool, args)
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out
}

func progressOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	p, ok := out["progress"].(map[string]any)
	require.True(t, ok, "expected progress: %v", out)
	return p
}

func parseTS(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "timestamp must be a string: %v", v)
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return ts
}

// publishedAt publishes one event and returns the timestamp the memo saw.
func publishedAt(t *testing.T, pub livepubsub.Publisher, kind string, payload any) time.Time {
	t.Helper()
	time.Sleep(2 * time.Millisecond) // distinct timestamps
	pub.Publish(context.Background(), "exec_p", kind, payload)
	a, ok := pub.(livepubsub.ActivityReader).Activity("exec_p")
	require.True(t, ok)
	return a.LastEventAt
}

func TestStatus_ProgressMidRun(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	publishedAt(t, f.pub, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "read", Role: "reader"})
	publishedAt(t, f.pub, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "read", Outcome: "success"})
	curateAt := publishedAt(t, f.pub, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "curate", Role: "worker"})

	p := progressOf(t, f.call(t, "status", nil))
	assert.EqualValues(t, 2, p["step_index"])
	assert.EqualValues(t, 3, p["step_count"])
	assert.Equal(t, "curate", p["step_id"])
	assert.EqualValues(t, 1, p["attempt"])
	assert.True(t, parseTS(t, p["step_started_at"]).Equal(curateAt), "step_started_at %v, want %v", p["step_started_at"], curateAt)
	assert.True(t, parseTS(t, p["last_event_at"]).Equal(curateAt))
}

func TestStatus_ProgressAbsentWhenTerminal(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	f.task.Status = persistence.TaskStatusCompleted
	out := f.call(t, "status", nil)
	assert.NotContains(t, out, "progress", "a terminal task has no running step")

	// A running task whose newest execution failed (its retry not started)
	// has no running step either.
	f.task.Status = persistence.TaskStatusRunning
	f.exec.Status = persistence.ExecutionStatusFailed
	assert.NotContains(t, f.call(t, "status", nil), "progress")

	// A task with no execution yet.
	f.exec = nil
	f.task.Status = persistence.TaskStatusQueued
	assert.NotContains(t, f.call(t, "status", nil), "progress")
}

func TestResult_NonTerminalCarriesProgress(t *testing.T) {
	f := newProgressFixture(t, true, "write")
	out := f.call(t, "result", map[string]any{"wait_seconds": 25})
	assert.Equal(t, false, out["complete"])
	p := progressOf(t, out)
	assert.EqualValues(t, 3, p["step_index"])
	assert.Equal(t, "write", p["step_id"])

	f.task.Status = persistence.TaskStatusCompleted
	done := f.call(t, "result", nil)
	assert.Equal(t, true, done["complete"])
	assert.NotContains(t, done, "progress")
}

func TestProgress_BrokerViewHasNoFreeText(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	curateAt := publishedAt(t, f.pub, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "curate"})
	// Content-bearing kinds the broker view drops must not move last_event_at:
	// a poller would otherwise count tool and model calls.
	publishedAt(t, f.pub, livepubsub.KindToolCallStarted, map[string]any{"tool": "gmail_search", "input_json": "secret query"})
	publishedAt(t, f.pub, livepubsub.KindLLMCallStarted, map[string]any{"model": "m"})

	p := progressOf(t, f.call(t, "status", nil))
	steps := threeStepWorkflow("x").Steps
	for k, v := range p {
		switch k {
		case "step_index", "step_count", "attempt":
			_, isNum := v.(float64)
			assert.True(t, isNum, "%s must be an integer: %v", k, v)
		case "step_started_at", "last_event_at":
			parseTS(t, v)
		case "step_id":
			_, declared := steps[v.(string)]
			assert.True(t, declared, "step_id %v must be a declared step", v)
		default:
			t.Errorf("unexpected progress field %q = %v", k, v)
		}
	}
	assert.True(t, parseTS(t, p["last_event_at"]).Equal(curateAt), "broker view: last_event_at must ignore dropped kinds")

	// A current step id the definition does not declare removes the object.
	for _, undeclared := range []string{"MERGE_FAILED", "done", ""} {
		f.exec.CurrentStepID = strp(undeclared)
		assert.NotContains(t, f.call(t, "status", nil), "progress", "current_step_id %q", undeclared)
	}
}

func TestProgress_CompanionViewLastEventCountsEveryKind(t *testing.T) {
	f := newProgressFixture(t, false, "curate")
	publishedAt(t, f.pub, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "curate"})
	toolAt := publishedAt(t, f.pub, livepubsub.KindToolCallStarted, map[string]any{"tool": "x"})
	p := progressOf(t, f.call(t, "status", nil))
	assert.True(t, parseTS(t, p["last_event_at"]).Equal(toolAt))
}

func TestProgress_OffChainStepHasNoIndex(t *testing.T) {
	f := newProgressFixture(t, true, "repair")
	p := progressOf(t, f.call(t, "status", nil))
	assert.Equal(t, "repair", p["step_id"])
	assert.EqualValues(t, 3, p["step_count"])
	assert.NotContains(t, p, "step_index", "a step reached only by on_fail has no position on the chain")
}

func TestProgress_StepStartedAtOnlyForTheRunningStep(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	// The memo still holds read's start (curate's step_started not seen, e.g.
	// on a replica that was down): no step_started_at rather than a wrong one.
	publishedAt(t, f.pub, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "read"})
	p := progressOf(t, f.call(t, "status", nil))
	assert.NotContains(t, p, "step_started_at")
	assert.Contains(t, p, "last_event_at")

	// No hub at all: the position still answers.
	f.srv.liveSub = nil
	p = progressOf(t, f.call(t, "status", nil))
	assert.NotContains(t, p, "step_started_at")
	assert.NotContains(t, p, "last_event_at")
	assert.EqualValues(t, 2, p["step_index"])
}

func TestProgress_SnapshotPreferredOverLiveDefinition(t *testing.T) {
	// The registry's live mail-digest has one step; the run was planned
	// against a three-step definition of the same id.
	f := newProgressFixture(t, true, "curate")
	wfID := "mail-digest"
	f.task.WorkflowID = &wfID
	f.exec.WorkflowID = wfID
	snap, _ := json.Marshal(threeStepWorkflow(wfID))
	f.exec.WorkflowSnapshot = snap
	p := progressOf(t, f.call(t, "status", nil))
	assert.EqualValues(t, 3, p["step_count"])
}

func TestProgress_HashMismatchOmitsProgress(t *testing.T) {
	f := newProgressFixture(t, true, "read")
	wfID := "mail-digest"
	f.task.WorkflowID = &wfID
	f.exec.WorkflowID = wfID
	f.exec.WorkflowSnapshot = nil
	f.exec.WorkflowRevision = "stale-revision"
	assert.NotContains(t, f.call(t, "status", nil), "progress", "the live graph is not the run's graph")

	// Positive control: the live definition is the one the run planned.
	live := f.srv.projectRegistry.GetWorkflow(wfID)
	require.NotNil(t, live)
	f.exec.WorkflowRevision = live.Hash()
	p := progressOf(t, f.call(t, "status", nil))
	assert.EqualValues(t, 1, p["step_count"])
	assert.EqualValues(t, 1, p["step_index"])
}

func TestCapabilities_AdvertisesCompanionProgress(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	rec := httptest.NewRecorder()
	srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	assert.False(t, decodeCapabilities(t, rec.Body.Bytes()).Features["companion-progress"], "no execution repository: no progress")

	srv.executionRepo = &mocks.MockExecutionRepository{}
	rec = httptest.NewRecorder()
	srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	assert.True(t, decodeCapabilities(t, rec.Body.Bytes()).Features["companion-progress"])
}

// GitHub #119 item 9 (2026-10-09, lane C C6 e2e run, H9a): the Postgres
// driver's GetByTaskID does not select workflow_snapshot (the SQLite one
// does), and the stored workflow_revision did not match the live hash, so on
// Postgres progress was absent: no snapshot, and no live fallback. The
// snapshot is read on its own.
func TestProgress_SnapshotReadWhenTheRowOmitsIt(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	snap := f.exec.WorkflowSnapshot
	f.exec.WorkflowSnapshot = nil // what Postgres GetByTaskID returns
	f.exec.WorkflowRevision = "v1"
	repo := f.srv.executionRepo.(*mocks.MockExecutionRepository)
	repo.GetWorkflowSnapshotFunc = func(_ context.Context, id string) ([]byte, error) {
		if id != "exec_p" {
			return nil, persistence.ErrNotFound
		}
		return snap, nil
	}
	p := progressOf(t, f.call(t, "status", nil))
	assert.EqualValues(t, 2, p["step_index"])
	assert.EqualValues(t, 3, p["step_count"])

	// review da32 F6: a failed snapshot read omits progress (the live
	// definition's hash does not match "v1"); status still answers.
	repo.GetWorkflowSnapshotFunc = func(context.Context, string) ([]byte, error) { return nil, assert.AnError }
	out := f.call(t, "status", nil)
	assert.Equal(t, "RUNNING", out["status"])
	assert.NotContains(t, out, "progress")
}

// review-20261008-82d8 F1: an execution read error is logged and omits
// progress; status still answers. (The C3 as-built note first claimed this
// test shipped with C2; it had not. Added 2026-10-09.)
func TestProgress_ExecutionReadErrorOmitsProgress(t *testing.T) {
	f := newProgressFixture(t, true, "curate")
	f.srv.executionRepo = &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(context.Context, string) (*persistence.Execution, error) {
			return nil, assert.AnError
		},
	}
	out := f.call(t, "status", nil)
	assert.Equal(t, "RUNNING", out["status"], "status still answers")
	assert.NotContains(t, out, "progress")

	// No execution repository at all, and no registry for the live fallback.
	f.srv.executionRepo = nil
	assert.NotContains(t, f.call(t, "status", nil), "progress")
	assert.Nil(t, (&Server{}).progressDefinition(context.Background(), &persistence.Execution{WorkflowID: "x", WorkflowRevision: "r"}))
}

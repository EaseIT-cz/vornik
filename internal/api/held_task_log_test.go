package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/secrets"
)

// GitHub #294 (2026-10-08): GET .../tasks/{id}/logs, .../explain, the support
// report, the UI log panel and the UI post-mortem reached Executor.TaskLogs,
// which ran `podman logs` on a request (process-spawn law). They now read
// HeldTaskLog: the live hub replay plus the persisted excerpt. These tests pin
// the reader and that no option can hand the API the executor's log accessor.

// spawningLogExecutor is an executor that ALSO has the old TaskLogs method.
// Before #294, WithExecutor type-asserted it into the server's log source, so
// GET .../logs ran it; the method fails the test if anything calls it.
type spawningLogExecutor struct {
	mockPauseResumeExecutor
	t *testing.T
}

func (e *spawningLogExecutor) TaskLogs(context.Context, string, int) (string, error) {
	e.t.Error("GitHub #294: a request reached Executor.TaskLogs, which runs podman logs")
	return "from podman logs", nil
}

type heldLogFixture struct {
	tasks map[string]*persistence.Task
	execs map[string]*persistence.Execution
	pub   livepubsub.Publisher
}

func newHeldLogFixture(ringSize int) *heldLogFixture {
	return &heldLogFixture{
		tasks: map[string]*persistence.Task{},
		execs: map[string]*persistence.Execution{},
		pub:   livepubsub.New(ringSize),
	}
}

func (f *heldLogFixture) taskRepo() *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		if tk, ok := f.tasks[id]; ok {
			cp := *tk
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}}
}

func (f *heldLogFixture) execRepo() *mocks.MockExecutionRepository {
	return &mocks.MockExecutionRepository{GetByTaskIDFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
		if e, ok := f.execs[id]; ok {
			cp := *e
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}}
}

func (f *heldLogFixture) reader() HeldTaskLog {
	return HeldTaskLog{Tasks: f.taskRepo(), Executions: f.execRepo(), Live: f.pub}
}

func (f *heldLogFixture) addTask(id, project, execID string, errMsg string) {
	f.tasks[id] = &persistence.Task{ID: id, ProjectID: project, Status: persistence.TaskStatusFailed}
	if execID != "" {
		e := &persistence.Execution{ID: execID, TaskID: id, ProjectID: project}
		if errMsg != "" {
			e.ErrorMessage = &errMsg
		}
		f.execs[id] = e
	}
}

func TestGetTaskLogs_NeverAsksTheExecutor(t *testing.T) {
	f := newHeldLogFixture(50)
	f.addTask("task-1", "project-1", "exec-1", "step failed: exit 1")
	f.pub.Publish(context.Background(), "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "build", Role: "coder"})

	server := NewServer(WithLogger(zerolog.Nop()),
		WithExecutor(&spawningLogExecutor{t: t}),
		WithTaskRepository(f.taskRepo()), WithExecutionRepository(f.execRepo()),
		WithLiveSubscriber(f.pub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/tasks/task-1/logs", nil)
	rec := httptest.NewRecorder()
	server.GetTaskLogs(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.NotContains(t, body, "from podman logs")
	assert.Contains(t, body, "step_started")
	assert.Contains(t, body, `"step_id":"build"`)
	assert.Contains(t, body, "step failed: exit 1")
}

func TestHeldTaskLog_HubEventsThenPersistedExcerpt(t *testing.T) {
	f := newHeldLogFixture(50)
	f.addTask("t1", "p1", "e1", "boom\nlast line")
	ctx := context.Background()
	f.pub.Publish(ctx, "e1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1"})
	f.pub.Publish(ctx, "e1", livepubsub.KindLLMToken, livepubsub.LLMTokenPayload{StepID: "s1", Delta: "tok"})
	f.pub.Publish(ctx, "e1", livepubsub.KindToolCallFinished, livepubsub.ToolCallFinishedPayload{CallID: "c1", DurationMs: 3})

	out, err := f.reader().TaskLogs(ctx, "t1", 100)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 5, out)
	assert.Contains(t, lines[0], " step_started ")
	assert.Contains(t, lines[1], " tool_call_finished ")
	assert.NotContains(t, out, "llm_token", "one event per token is noise; llm_call_finished carries the call")
	assert.Equal(t, heldLogPersistedMarker, lines[2])
	assert.Equal(t, "boom", lines[3])
	assert.Equal(t, "last line", lines[4])
}

func TestHeldTaskLog_TailKeepsTheNewestLines(t *testing.T) {
	f := newHeldLogFixture(50)
	f.addTask("t1", "p1", "e1", "one\ntwo\nthree")
	out, err := f.reader().TaskLogs(context.Background(), "t1", 2)
	require.NoError(t, err)
	assert.Equal(t, "two\nthree", out)
}

func TestHeldTaskLog_ReplayGapIsNamed(t *testing.T) {
	f := newHeldLogFixture(2)
	f.addTask("t1", "p1", "e1", "")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f.pub.Publish(ctx, "e1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s"})
	}
	out, err := f.reader().TaskLogs(ctx, "t1", 100)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 3, out)
	assert.Contains(t, lines[0], "earlier events are no longer held")
}

func TestHeldTaskLog_PayloadIsCapped(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "")
	f.pub.Publish(context.Background(), "e1", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: strings.Repeat("a", 5000)})
	out, err := f.reader().TaskLogs(context.Background(), "t1", 100)
	require.NoError(t, err)
	assert.Less(t, len(out), heldLogPayloadCap+200)
	assert.True(t, strings.HasSuffix(out, "…"), out)
}

func TestHeldTaskLog_NoExecutionFallsBackToLastError(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "", "")
	le := "never started: quota"
	f.tasks["t1"].LastError = &le
	out, err := f.reader().TaskLogs(context.Background(), "t1", 100)
	require.NoError(t, err)
	assert.Equal(t, "never started: quota", out)
}

func TestHeldTaskLog_NothingHeldIsAnError(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "")
	_, err := f.reader().TaskLogs(context.Background(), "t1", 100)
	require.Error(t, err)

	_, err = f.reader().TaskLogs(context.Background(), "", 100)
	require.Error(t, err)
}

func TestHeldTaskLog_NilSourcesDegrade(t *testing.T) {
	_, err := HeldTaskLog{}.TaskLogs(context.Background(), "t1", 10)
	require.Error(t, err)
}

func TestHeldTaskLog_TailCutsAcrossTheJoin(t *testing.T) {
	f := newHeldLogFixture(50)
	f.addTask("t1", "p1", "e1", "boom")
	f.pub.Publish(context.Background(), "e1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1"})
	f.pub.Publish(context.Background(), "e1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2"})
	out, err := f.reader().TaskLogs(context.Background(), "t1", 3)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 3, out)
	assert.Contains(t, lines[0], `"step_id":"s2"`)
	assert.Equal(t, heldLogPersistedMarker, lines[1])
	assert.Equal(t, "boom", lines[2])
}

// The support report reads uncapped, so its redaction sees whole values
// (review-20261008-3356 finding 3).
func TestHeldTaskLog_UncappedKeepsWholePayloads(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "")
	f.pub.Publish(context.Background(), "e1", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: strings.Repeat("a", 2000) + "END"})
	r := f.reader()
	r.Uncapped = true
	out, err := r.TaskLogs(context.Background(), "t1", 100)
	require.NoError(t, err)
	assert.Contains(t, out, "END")
}

func TestHeldTaskLog_ExecutionOfAnotherProjectIsNotRead(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "foreign error")
	f.execs["t1"].ProjectID = "p2"
	f.pub.Publish(context.Background(), "e1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1"})
	_, err := f.reader().TaskLogs(context.Background(), "t1", 100)
	require.Error(t, err, "a row of another project must not be served")
}

func TestSupportReport_TaskLogIsUncapped(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "")
	f.pub.Publish(context.Background(), "e1", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: strings.Repeat("a", 2000) + "END-OF-OUTPUT"})
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	s := NewServer(WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithSecrets(det, secrets.DefaultCheckpoints()),
		WithTaskRepository(f.taskRepo()), WithExecutionRepository(f.execRepo()), WithLiveSubscriber(f.pub))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)), "sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	files := extractBundle(t, rec.Body.Bytes())
	assert.Contains(t, string(files["task/task_log.txt"]), "END-OF-OUTPUT")
}

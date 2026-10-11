package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// failedExecRepo answers List with one FAILED execution of the task, only
// when the filter asks for the task's FAILED executions.
func failedExecRepo(t *testing.T, exec *persistence.Execution) *mocks.MockExecutionRepository {
	t.Helper()
	return &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			if f.TaskID == nil || *f.TaskID != statusTaskID || f.Status == nil || *f.Status != persistence.ExecutionStatusFailed {
				return nil, nil
			}
			if exec == nil {
				return nil, nil
			}
			return []*persistence.Execution{exec}, nil
		},
	}
}

func statusOut(t *testing.T, srv *Server, raw string) (map[string]any, string) {
	t.Helper()
	text, isErr := callTool(t, srv, raw, "status", map[string]any{"task_id": statusTaskID})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out, text
}

// GitHub #98 (2026-10-03 customer zero): while a retry ran, status said only
// RUNNING; the agent could not see that the first attempt had failed, nor where.
func TestCompanionStatus_RunningRetryReportsLastAttempt(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	wf := "wf-alpha"
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		// The requeue cleared the class (by design) and stored the step error.
		return &persistence.Task{ID: "t98", ProjectID: "alpha", WorkflowID: &wf,
			Status: persistence.TaskStatusRunning, Attempt: 2, MaxAttempts: 3,
			LastError: strp("step report: x")}, nil
	}
	srv.executionRepo = failedExecRepo(t, &persistence.Execution{
		ID: "exec-1", TaskID: "t98", ProjectID: "alpha", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"), ErrorMessage: strp("x"), ErrorCode: strp("TOOL_ERROR"),
	})

	out, _ := statusOut(t, srv, raw)
	assert.EqualValues(t, 2, out["attempt"])
	la, ok := out["last_attempt"].(map[string]any)
	require.True(t, ok, "status must carry last_attempt while the retry runs: %v", out)
	assert.EqualValues(t, 1, la["attempt"])
	assert.Equal(t, "report", la["failed_step"])
	assert.Equal(t, "TOOL_ERROR", la["error_class"])
}

// GitHub #98 (2026-10-08): a terminal FAILED task reports its own final attempt.
func TestCompanionStatus_FailedTaskLastAttemptIsCurrentAttempt(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t98", ProjectID: "alpha", Status: persistence.TaskStatusFailed, Attempt: 3, MaxAttempts: 3}, nil
	}
	srv.executionRepo = failedExecRepo(t, &persistence.Execution{
		ID: "exec-3", TaskID: "t98", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"), ErrorCode: strp("SETUP_INCOMPLETE"),
	})
	out, _ := statusOut(t, srv, raw)
	la, ok := out["last_attempt"].(map[string]any)
	require.True(t, ok, "%v", out)
	assert.EqualValues(t, 3, la["attempt"])
	assert.Equal(t, "SETUP_INCOMPLETE", la["error_class"])
}

// GitHub #98 (2026-10-08): a first attempt that is still running has no last_attempt.
func TestCompanionStatus_FirstAttemptHasNoLastAttempt(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t98", ProjectID: "alpha", Status: persistence.TaskStatusRunning, Attempt: 1, MaxAttempts: 3}, nil
	}
	srv.executionRepo = failedExecRepo(t, &persistence.Execution{ID: "old", TaskID: "t98", Status: persistence.ExecutionStatusFailed})
	out, _ := statusOut(t, srv, raw)
	_, has := out["last_attempt"]
	assert.False(t, has)
}

// GitHub #98 (2026-10-08): broker egress rule. A broker-project key reading an
// ordinary task (review-20260929-388f H1 shape) gets a closed broker class and
// a step id only; the execution's raw error never appears.
func TestCompanionStatus_BrokerProjectKeyLastAttemptHasNoRawError(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	wf := "wf-plain"
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t98", ProjectID: "broker-acme", WorkflowID: &wf,
			Status: persistence.TaskStatusRunning, Attempt: 2, MaxAttempts: 3,
			LastError: strp("step report: " + brokerSentinel)}, nil
	}
	srv.executionRepo = failedExecRepo(t, &persistence.Execution{
		ID: "exec-1", TaskID: "t98", ProjectID: "broker-acme", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"), ErrorMessage: strp("request timed out reading " + brokerSentinel),
		ErrorCode: strp("TOOL_ERROR"),
	})

	out, text := statusOut(t, srv, raw)
	la, ok := out["last_attempt"].(map[string]any)
	require.True(t, ok, text)
	assert.Equal(t, "report", la["failed_step"])
	// review-20261008-a4ef F5: the broker class comes from the closed execution
	// class (TOOL_ERROR -> failed), never from the error text ("timed out").
	assert.Equal(t, brokerErrFailed, la["error_class"], "closed broker class from the closed execution class")
	assert.Contains(t, []string{brokerErrFailed, brokerErrTimeout, brokerErrBudget}, la["error_class"])
	assert.NotContains(t, text, brokerSentinel)
	assert.NotContains(t, text, "last_error")
	assert.NotContains(t, text, "TOOL_ERROR", "the executor class is not the broker vocabulary")
}

// GitHub #98 (2026-10-08): an execution read error omits last_attempt; status still answers.
func TestCompanionStatus_LastAttemptReadErrorOmitsKey(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t98", ProjectID: "alpha", Status: persistence.TaskStatusRunning, Attempt: 2}, nil
	}
	srv.executionRepo = &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, assert.AnError
		},
	}
	out, _ := statusOut(t, srv, raw)
	_, has := out["last_attempt"]
	assert.False(t, has)
	assert.Equal(t, "RUNNING", out["status"])
}

func strp(s string) *string { return &s }

// statusTaskID is the task every status test here reads.
const statusTaskID = "t98"

// GitHub #92 (2026-10-08, review-20261008-5b0e F2): last_attempt.error_class is
// closed at the source; the executor's EXECUTION_ERROR fallback (or anything
// outside the task failure vocabulary) reads UNKNOWN.
func TestCompanionStatus_LastAttemptClassIsClosed(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: statusTaskID, ProjectID: "alpha", Status: persistence.TaskStatusRunning, Attempt: 2}, nil
	}
	srv.executionRepo = failedExecRepo(t, &persistence.Execution{ID: "e", TaskID: statusTaskID,
		Status: persistence.ExecutionStatusFailed, ErrorCode: strp("EXECUTION_ERROR")})
	out, _ := statusOut(t, srv, raw)
	la, ok := out["last_attempt"].(map[string]any)
	require.True(t, ok, "%v", out)
	assert.Equal(t, persistence.TaskFailureClassUnknown, la["error_class"])
}

// GitHub #92 (2026-10-08), live design Change C2: status shares the three-way
// broker rule. An agent-admin key whose home project is not loaded reads a
// task in a broker project of its namespace through an ordinary workflow:
// neither of the first two conditions holds, the task's project does.
func TestCompanionStatus_TaskOnBrokerProjectGetsBrokerShape(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"hermes--fin": {ID: "hermes--fin", Broker: true}})
	keyRepo := &memAPIKeyRepo{}
	taskRepo := &mocks.MockTaskRepository{}
	srv := &Server{logger: zerolog.Nop(), apiKeyRepo: keyRepo, taskRepo: taskRepo, projectRegistry: reg}
	raw, _ := seedCompanionKey(t, keyRepo, "hermes--home", nil)
	keyRepo.rows[len(keyRepo.rows)-1].AgentAdmin = true
	keyRepo.rows[len(keyRepo.rows)-1].AgentNamespace = "hermes"
	wf := "wf-plain"
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: statusTaskID, ProjectID: "hermes--fin", WorkflowID: &wf,
			Status: persistence.TaskStatusFailed, Attempt: 1, LastError: strp("timed out on " + brokerSentinel),
			// GitHub #299: the broker class reads the stored class, not the text.
			LastErrorClass: strp(persistence.TaskFailureClassTimeout)}, nil
	}
	out, text := statusOut(t, srv, raw)
	assert.NotContains(t, text, brokerSentinel)
	_, hasRaw := out["last_error"]
	assert.False(t, hasRaw, text)
	assert.Equal(t, brokerErrTimeout, out["error_class"])
}

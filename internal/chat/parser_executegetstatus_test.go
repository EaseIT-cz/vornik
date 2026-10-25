// Coverage for executeGetStatus's execution-repo branches that
// parser_execute_test.go skipped (LastError, exec.ErrorMessage,
// exec.Result truncation). Lifts the function from 62.9% to ~95%.

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/report"
)

// rawErrorView is the caller's chosen view of a stored error text that
// leaves it unchanged (the dispatcher's operator view, #480).
func rawErrorView(s string) string { return s }

// GitHub #480: a caller that chose no view never gets the raw text; with a
// nil ErrorView every line of last_error and error_message is withheld.
func TestExecuteGetStatus480_NilErrorViewWithholds(t *testing.T) {
	le := "auth failed\nexit status 1"
	em := "token rejected"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusFailed, LastError: &le}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: "exec-1", ErrorMessage: &em}, nil
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionGetStatus, TaskID: "t-1"}, taskRepo, execRepo, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	for _, raw := range []string{"auth failed", "exit status 1", "token rejected"} {
		if strings.Contains(res.Message, raw) {
			t.Errorf("nil ErrorView served raw text %q: %q", raw, res.Message)
		}
	}
	if !strings.Contains(res.Message, "Last Error: [withheld: secret]") || !strings.Contains(res.Message, "Error: [withheld: secret]") {
		t.Errorf("nil ErrorView did not withhold: %q", res.Message)
	}
}

func TestExecuteGetStatus_WithLastError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			le := "previous run crashed"
			return &persistence.Task{
				ID: id, Status: persistence.TaskStatusFailed,
				Attempt: 3, MaxAttempts: 3,
				LastError: &le,
			}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1", ErrorView: rawErrorView}, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success || !strings.Contains(res.Message, "Last Error: previous run crashed") {
		t.Errorf("LastError missing from summary: %q", res.Message)
	}
}

func TestExecuteGetStatus_WithExecution(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	resultMsg := map[string]string{"message": "completed work"}
	resultBytes, _ := json.Marshal(resultMsg)
	emsg := "warning: low fuel"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:           "exec-1",
				Status:       persistence.ExecutionStatusCompleted,
				ErrorMessage: &emsg,
				Result:       resultBytes,
			}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1", ErrorView: rawErrorView}, taskRepo, execRepo, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !strings.Contains(res.Message, "Execution: exec-1") {
		t.Errorf("execution ID missing: %q", res.Message)
	}
	if !strings.Contains(res.Message, "completed work") {
		t.Errorf("execution result message missing: %q", res.Message)
	}
	if !strings.Contains(res.Message, "Error: warning: low fuel") {
		t.Errorf("execution error missing: %q", res.Message)
	}
}

func TestExecuteGetStatus_WithExecution_TruncatesLongResult(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	long := strings.Repeat("a", 1000)
	resultBytes, _ := json.Marshal(map[string]string{"message": long})
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{Result: resultBytes}, nil
		},
	}
	// #497: the agent output takes the error view; the zero value withholds
	// every line, so this test chooses the operator view to see the cut.
	res, _ := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1", ErrorView: rawErrorView}, taskRepo, execRepo, nil)
	if !strings.Contains(res.Message, "(truncated)") {
		t.Errorf("long result not truncated: %q", res.Message)
	}
}

// GitHub #497 (operator decision A, process-spawn-law design, "Amendment
// 2026-10-24 (#497)"): the input prompt and the agent's output excerpt take
// the same view as the error text, applied before the 500-byte cut.
func TestExecuteGetStatus497_PromptAndResultTakeTheView(t *testing.T) {
	promptSecret := "ghp_" + strings.Repeat("P1aS2dF3gH", 4)[:36]
	resultSecret := "ghp_" + strings.Repeat("R5tY8uI1oP", 4)[:36]
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted,
				Payload: []byte(`{"context":{"prompt":"deploy with token ` + promptSecret + `"}}`)}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			// The secret straddles the 500-byte cut.
			msg := strings.Repeat("x", 480) + " " + resultSecret + "\ndone"
			res, _ := json.Marshal(map[string]string{"message": msg})
			return &persistence.Execution{ID: "exec-1", Result: res}, nil
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionGetStatus, TaskID: "t-1", ErrorView: func(s string) string {
		out, _ := report.ScrubLines(s, func(line string) (string, bool) {
			if strings.Contains(line, "ghp_") {
				return "", true
			}
			return line, false
		})
		return out
	}}, taskRepo, execRepo, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if strings.Contains(res.Message, "ghp_") {
		t.Errorf("a secret or a fragment of one reached the model: %q", res.Message)
	}
	if !strings.Contains(res.Message, report.WithheldLine) {
		t.Errorf("no withheld marker: %q", res.Message)
	}
	raw, err := ExecuteAction(context.Background(), Action{Type: ActionGetStatus, TaskID: "t-1", ErrorView: rawErrorView}, taskRepo, execRepo, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !strings.Contains(raw.Message, promptSecret) {
		t.Errorf("the operator view lost the raw prompt: %q", raw.Message)
	}
}

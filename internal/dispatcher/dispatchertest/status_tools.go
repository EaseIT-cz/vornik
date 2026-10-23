// Package dispatchertest holds fixtures for tests that drive the chat
// dispatcher through a channel's inbound path with a scripted model. It is
// imported only by _test.go files.
//
// GitHub #480 (process-spawn-law design, "Amendment 2026-10-21 (#480)"): the
// task tools give the model the stored error text, scrubbed unless the turn's
// audience is an admin-linked principal alone. Every channel's test seeds the
// same planted secrets and scripts the same tool calls, so they share these.
package dispatchertest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// The planted secrets: a GitHub token after "Bearer", an AWS key on its own
// line, and a Slack key after a literal escaped newline (backslash, n), as a
// provider's quoted JSON error body carries it (#466 B3).
var (
	Token480 = "ghp_" + strings.Repeat("Q4wE7rT0yU", 4)[:36]
	AWS480   = "AKIA" + "IOSFODNN7EXAMPLZ"
	Slack480 = "xoxb-" + "4801234567-abcdefghij"

	// ExecToken480 appears only in the execution's error_message.
	ExecToken480 = "ghp_" + strings.Repeat("E8xC3vB6nM", 4)[:36]

	// ErrText480 is the task's last_error: three secret-holding lines and a
	// clean one, which every view keeps.
	ErrText480 = "step build failed: Authorization: Bearer " + Token480 +
		"\n" + AWS480 +
		"\n{\"error\":\"denied\\n" + Slack480 + "\"}" +
		"\nexit status 1"
)

// CleanLine480 is the line of ErrText480 that holds no secret.
const CleanLine480 = "exit status 1"

// TaskID480 and ExecID480 name the seeded rows.
const (
	TaskID480 = "task_20261021000000_0000000000000480"
	ExecID480 = "exec-480"
)

// Secrets480 lists every planted value, for absence checks.
func Secrets480() []string {
	return []string{Token480, AWS480, Slack480, ExecToken480}
}

// SeedFailedTask480 creates a FAILED task in project whose last_error is
// ErrText480 and whose execution's error_message holds ExecToken480.
func SeedFailedTask480(t *testing.T, tasks persistence.TaskRepository, execs persistence.ExecutionRepository, project string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	wf := "wf-480"
	le := ErrText480
	if err := tasks.Create(ctx, &persistence.Task{
		ID: TaskID480, ProjectID: project, WorkflowID: &wf, Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), LastError: &le, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	em := "container exited: token " + ExecToken480 + "\n" + CleanLine480
	start := now.Add(-time.Minute)
	if err := execs.Create(ctx, &persistence.Execution{
		ID: ExecID480, TaskID: TaskID480, ProjectID: project, WorkflowID: wf,
		Status: persistence.ExecutionStatusFailed, ErrorMessage: &em, StartedAt: &start, CompletedAt: &now,
	}); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
}

// StatusModel is an OpenAI-compatible model server whose first reply calls
// get_task_status and wait_for_task on TaskID480, and whose later replies
// are plain text. ToolResults returns what the dispatcher handed back to the
// model as tool messages, in order.
type StatusModel struct {
	URL string

	mu   sync.Mutex
	reqs []chat.ChatRequest
}

// NewStatusModel starts the scripted model server.
func NewStatusModel(t *testing.T) *StatusModel {
	t.Helper()
	m := &StatusModel{}
	args, _ := json.Marshal(map[string]any{"task_id": TaskID480, "timeout_seconds": 1})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chat.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		n := len(m.reqs)
		m.mu.Unlock()
		msg := map[string]any{"role": "assistant", "content": "The task failed."}
		finish := "stop"
		if n == 1 {
			msg = map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []map[string]any{
					{"id": "c480a", "type": "function", "function": map[string]any{"name": "get_task_status", "arguments": string(args)}},
					{"id": "c480b", "type": "function", "function": map[string]any{"name": "wait_for_task", "arguments": string(args)}},
				},
			}
			finish = "tool_calls"
		}
		body, _ := json.Marshal(map[string]any{
			"id": "r", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	m.URL = srv.URL
	return m
}

// Provider returns a chat client pointed at the scripted server.
func (m *StatusModel) Provider() chat.Provider {
	return chat.NewClient(m.URL, "k", "m")
}

// Requests returns every request the model was sent, in order.
func (m *StatusModel) Requests() []chat.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]chat.ChatRequest(nil), m.reqs...)
}

// ToolResults returns the content of every tool message the model was sent.
func (m *StatusModel) ToolResults() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, req := range m.reqs {
		for _, msg := range req.Messages {
			if msg.Role != "tool" || seen[msg.ToolCallID] {
				continue
			}
			seen[msg.ToolCallID] = true
			out = append(out, msg.Content)
		}
	}
	return out
}

// AssertScrubbed fails unless both status tools ran, no planted secret
// reached the model, and the clean line and the withheld marker did.
func AssertScrubbed(t *testing.T, where string, results []string) {
	t.Helper()
	assertBothTools(t, where, results)
	joined := strings.Join(results, "\n")
	for _, s := range Secrets480() {
		if strings.Contains(joined, s) {
			t.Errorf("%s: a planted secret reached the chat model: %q", where, s)
		}
	}
	if !strings.Contains(joined, "[withheld: secret]") {
		t.Errorf("%s: no line was withheld:\n%s", where, joined)
	}
	if !strings.Contains(joined, CleanLine480) {
		t.Errorf("%s: the clean line %q was lost:\n%s", where, CleanLine480, joined)
	}
}

// AssertRaw fails unless both status tools ran and the model was handed the
// raw error text (the admin-linked view).
func AssertRaw(t *testing.T, where string, results []string) {
	t.Helper()
	assertBothTools(t, where, results)
	joined := strings.Join(results, "\n")
	for _, s := range Secrets480() {
		if !strings.Contains(joined, s) {
			t.Errorf("%s: the admin-linked view lost the raw value %q:\n%s", where, s, joined)
		}
	}
}

// AssertOperatorView is AssertRaw for a production-wired dispatcher, whose
// output guard (WithOutputGuard) redacts some secret shapes in place in
// every tool result, whatever the view. It fails unless no line was
// withheld: the secret-holding lines are served as lines (the guard may
// have masked a value inside one), which is the admin-linked view.
func AssertOperatorView(t *testing.T, where string, results []string) {
	t.Helper()
	assertBothTools(t, where, results)
	joined := strings.Join(results, "\n")
	if strings.Contains(joined, "[withheld: secret]") {
		t.Errorf("%s: the admin-linked view withheld a line:\n%s", where, joined)
	}
	for _, line := range []string{"step build failed: Authorization: Bearer", "container exited: token", "{\"error\":\"denied"} {
		if !strings.Contains(joined, line) {
			t.Errorf("%s: the admin-linked view lost the line starting %q:\n%s", where, line, joined)
		}
	}
}

func assertBothTools(t *testing.T, where string, results []string) {
	t.Helper()
	if len(results) != 2 {
		t.Fatalf("%s: want 2 tool results (get_task_status, wait_for_task), got %d: %q", where, len(results), results)
	}
	if !strings.Contains(results[0], "Last Error:") || !strings.Contains(results[1], "Last error:") {
		t.Fatalf("%s: the status tools did not render the error field: %q", where, results)
	}
}

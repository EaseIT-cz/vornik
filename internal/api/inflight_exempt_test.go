package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #97, implementation review 5860 finding #2 (controller ruling
// 2026-10-08): with the drain refusal an agent step's LLM call in the
// shutdown grace got 503 and the step could FAIL instead of being paused by
// Phase 2. A request authenticated with the executor-minted per-task key of
// a RUNNING task is exempt (and still counted); nothing a caller can set in
// a header grants the exemption. Design: 2026-10-08-deploy-idle-gate-design.md §8.5a.

func taskStatusRepo(status persistence.TaskStatus, err error) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		if err != nil {
			return nil, err
		}
		return &persistence.Task{ID: id, ProjectID: "p", Status: status}, nil
	}}
}

// executorTaskKey is the row taskkeys.MintTaskKey writes for task_1.
func executorTaskKey() *persistence.APIKey {
	return &persistence.APIKey{ID: "key_1", ProjectID: "p", Name: persistence.TaskKeyNamePrefix + "task_1", CreatedBy: "executor"}
}

func drainedRequest(row *persistence.APIKey, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", chatBody())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if row != nil {
		ctx := context.WithValue(req.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}})
		req = req.WithContext(ctx)
	}
	return req
}

func TestInflight_DrainServesRunningTaskKeyAndCountsIt(t *testing.T) {
	prov := newBlockingProvider()
	s := &Server{logger: zerolog.Nop(), chatProvider: prov, taskRepo: taskStatusRepo(persistence.TaskStatusRunning, nil)}
	s.SetDraining(true)
	h := s.trackLongRequest(s.ChatCompletions)
	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h(w, drainedRequest(executorTaskKey(), nil))
		done <- w.Code
	}()
	select {
	case <-prov.entered:
	case code := <-done:
		t.Fatalf("a running step's call during the drain was answered %d without reaching the provider, want it served", code)
	case <-time.After(5 * time.Second):
		t.Fatal("the request neither reached the provider nor returned")
	}
	if got := s.InflightChatProxy(); got != 1 {
		t.Fatalf("an exempt request must still be counted: in-flight = %d", got)
	}
	close(prov.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("a running step's call during the drain = %d, want 200", code)
	}
	if got := s.InflightChatProxy(); got != 0 {
		t.Fatalf("in-flight after = %d, want 0", got)
	}
}

func TestInflight_DrainServesLeasedTaskKey(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: okStub(), taskRepo: taskStatusRepo(persistence.TaskStatusLeased, nil)}
	s.SetDraining(true)
	w := httptest.NewRecorder()
	s.trackLongRequest(s.ChatCompletions)(w, drainedRequest(executorTaskKey(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("a LEASED task's key during the drain = %d %s, want 200", w.Code, w.Body.String())
	}
}

func refusedWhileDrained(t *testing.T, s *Server, req *http.Request, what string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.trackLongRequest(s.ChatCompletions)(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "DRAINING") {
		t.Errorf("%s while draining = %d %s, want 503 DRAINING", what, w.Code, w.Body.String())
	}
}

func TestInflight_DrainRefusesExternalAndCompanionKeys(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: okStub(), taskRepo: taskStatusRepo(persistence.TaskStatusRunning, nil)}
	s.SetDraining(true)
	refusedWhileDrained(t, s, drainedRequest(nil, nil), "an unauthenticated request")
	refusedWhileDrained(t, s, drainedRequest(&persistence.APIKey{Name: "ci-bot", CreatedBy: "akey_x"}, nil), "an ordinary key")
	refusedWhileDrained(t, s, drainedRequest(&persistence.APIKey{Name: persistence.TaskKeyNamePrefix + "task_1", ClientKind: "claude-code", CreatedBy: "executor"}, nil), "a companion key named like a task key")
	refusedWhileDrained(t, s, drainedRequest(&persistence.APIKey{Name: persistence.TaskKeyNamePrefix + "task_1", CreatedBy: "akey_operator"}, nil), "a rotated task key")
	refusedWhileDrained(t, s, drainedRequest(&persistence.APIKey{Name: "agent:warm_p:writer", CreatedBy: "executor-warm"}, nil), "a warm-pool key")
}

func TestInflight_DrainRefusesForgedExecutionHeader(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: okStub(), taskRepo: taskStatusRepo(persistence.TaskStatusRunning, nil)}
	s.SetDraining(true)
	refusedWhileDrained(t, s, drainedRequest(&persistence.APIKey{Name: "ci-bot", CreatedBy: "akey_x"}, map[string]string{
		"X-Vornik-Task-ID": "task_1", "X-Vornik-Execution-ID": "exec_1", "X-Vornik-Project-ID": "p",
	}), "a non-agent key with forged task and execution headers")
	refusedWhileDrained(t, s, drainedRequest(nil, map[string]string{
		"X-Vornik-Task-ID": "task_1", "X-Vornik-Execution-ID": "exec_1",
	}), "headers alone")
}

func TestInflight_DrainRefusesTaskKeyOfFinishedTask(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: okStub(), taskRepo: taskStatusRepo(persistence.TaskStatusCompleted, nil)}
	s.SetDraining(true)
	refusedWhileDrained(t, s, drainedRequest(executorTaskKey(), nil), "a finished task's key")
	s.taskRepo = taskStatusRepo("", errors.New("db down"))
	refusedWhileDrained(t, s, drainedRequest(executorTaskKey(), nil), "a task key when the task read fails")
	s.taskRepo = nil
	refusedWhileDrained(t, s, drainedRequest(executorTaskKey(), nil), "a task key with no task repository")
}

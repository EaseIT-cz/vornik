package ui

// GitHub #440 (security audit 2026-10-17): the UI log panel served the
// operator view of the held task log to every caller the project check
// admits, so a project-scoped key read through /ui/tasks/{id}/logs/stream
// what the scrubbed REST route withholds. Found at: review (code audit); the
// test drives the SSE handler itself. An admin-class caller keeps the
// operator view (process-spawn-law design, "Amendment 2026-10-21 (#439,
// #440)", C3 R4).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/report"
)

var planted440UI = "ghp_" + strings.Repeat("M4nB7vC1xZ", 4)[:36]

func panel440(t *testing.T) *Server {
	t.Helper()
	errMsg := "step build failed\nGITHUB_TOKEN=" + planted440UI + "\nexit status 1"
	tasks := &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "alpha", Status: persistence.TaskStatusFailed}, nil
	}}
	execs := &mocks.MockExecutionRepository{GetByTaskIDFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: "e1", TaskID: id, ProjectID: "alpha", ErrorMessage: &errMsg}, nil
	}}
	held := api.HeldTaskLog{Tasks: tasks, Executions: execs}
	scrub, err := report.NewLineScrubber("")
	require.NoError(t, err)
	scrubbed, err := held.ScrubbedView(scrub)
	require.NoError(t, err)
	return NewServer(
		WithTaskRepository(tasks), WithExecutionRepository(execs),
		WithTaskLogSource(held), WithScrubbedTaskLogSource(scrubbed),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-ui"}}),
	)
}

func stream440(ctx context.Context, t *testing.T, srv *Server) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.TaskLogsStream(rec, req, "t1")
	return rec.Body.String()
}

func TestTaskLogsStream440_ProjectKeyReadsScrubbed(t *testing.T) {
	srv := panel440(t)

	body := stream440(api.ContextWithProjectScope(context.Background(), "alpha"), t, srv)
	assert.NotContains(t, body, planted440UI, "a project-scoped caller read the planted secret on the UI log panel")
	assert.Contains(t, body, report.WithheldLine)
	assert.Contains(t, body, "exit status 1")

	admin := api.ContextWithAPIKeyForTesting(api.ContextWithProjectScope(context.Background(), "*"), "sk-admin-ui")
	assert.Contains(t, stream440(admin, t, srv), planted440UI, "the operator view must stay unchanged")
}

// With no scrubbed source wired, a non-admin caller gets no log text at all,
// and in particular not the raw error_message fallback.
func TestTaskLogsStream440_NoScrubbedSourceServesNothing(t *testing.T) {
	srv := panel440(t)
	srv.scrubbedTaskLogSource = nil
	body := stream440(api.ContextWithProjectScope(context.Background(), "alpha"), t, srv)
	assert.NotContains(t, body, planted440UI)
	assert.Contains(t, body, "No logs available yet.")
}

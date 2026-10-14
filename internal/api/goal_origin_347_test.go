package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/taskcreate"
)

// GitHub #347 (learning-loop design, amendment 2026-10-13): every task row
// records the surface that wrote its goal, as a constant the creating code
// chooses. A goal_origin key in the request is payload, never the marker.

func TestCreateTask_InlinePathStampsRESTGoalOrigin(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))
	body := `{"taskType":"test","context":{"prompt":"x","goal_origin":"companion"}}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, persistence.TaskGoalOriginREST, taskRepo.LastCall.Task.GoalOrigin)
}

func TestCreateTask_CreatorPathStampsRESTGoalOrigin(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(taskcreate.WithTaskRepository(taskRepo), taskcreate.WithProjectRegistry(reg))
	server := NewServer(WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithTaskCreator(creator))
	body := `{"taskType":"research","context":{"prompt":"x","goal_origin":"companion"}}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, persistence.TaskGoalOriginREST, taskRepo.LastCall.Task.GoalOrigin)
}

// The webhook sender writes the goal. A body claiming a first-party origin
// is still a webhook goal.
func TestIngestWebhook_StampsWebhookGoalOriginWhateverTheBodySays(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-347","goal_origin":"rest","issue":{"title":"Fix login"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, taskRepo.CallCount.Create)
	require.Equal(t, persistence.TaskGoalOriginWebhook, taskRepo.LastCall.Task.GoalOrigin)
}

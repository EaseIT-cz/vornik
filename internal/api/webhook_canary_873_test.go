package api

// GitHub #873, github-app-channel design C1: a canary task runs the App review
// on a planted diff and needs no forge job, so a webhook could start unbounded
// canary runs. The generic webhook refuses a rendered task type of canary.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence/mocks"
)

func TestWebhook_873_RefusesTheCanaryTaskType(t *testing.T) {
	for _, tmpl := range []string{"canary", " canary ", "CANARY", "${action}"} {
		reg := testWebhookRegistry(t)
		taskRepo := &mocks.MockTaskRepository{}
		webhookRepo := &mockWebhookEventRepo{}
		server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo),
			WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
		project := reg.GetProject("project-1")
		source, ok := findWebhookSource(project, "github")
		require.True(t, ok)
		source.TaskTypeTemplate = tmpl

		rec := httptest.NewRecorder()
		server.enqueueVerifiedWebhook(context.Background(), rec, project, source, []byte(`{"id":"evt-c","action":"canary"}`), "d-c")

		require.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", tmpl, rec.Body.String())
		assert.Equal(t, 0, taskRepo.CallCount.Create, tmpl)
		require.Len(t, webhookRepo.events, 1)
		assert.Equal(t, "reserved_task_type", webhookRepo.events[0].ErrorCode)
	}
}

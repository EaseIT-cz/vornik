package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/secrets"
)

// Secret-leak Phase 3 follow-up (design 2026-07-11, "the remaining sinks",
// 2026-10-01): the webhook and backlog-deposit checkpoints logged their
// findings but recorded nothing to secret_redaction_audit.

type sinkRecorder struct {
	mu     sync.Mutex
	events []persistence.SecretRedactionEvent
}

func (r *sinkRecorder) Record(_ context.Context, ev []persistence.SecretRedactionEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev...)
	return nil
}

func TestWebhookSecretScan_RecordsTheFinding(t *testing.T) {
	reg := testWebhookRegistry(t)
	server := newSecretsServer(t, reg, &mocks.MockTaskRepository{}, &mockWebhookEventRepo{})
	rec := &sinkRecorder{}
	server.SetSecretRedactionAudit(rec)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	router.Handler().ServeHTTP(httptest.NewRecorder(), req)

	require.NotEmpty(t, rec.events, "the blocked webhook's finding must be recorded")
	ev := rec.events[0]
	assert.Equal(t, "project-1", ev.ProjectID)
	assert.Equal(t, secrets.CheckpointWebhook, ev.Checkpoint)
	assert.Equal(t, "live", ev.Source)
	assert.Empty(t, ev.TaskID, "a webhook has no task yet")
}

func TestBacklogDepositSecretScan_RecordsTheFinding(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	rec := &sinkRecorder{}
	fx.server.SetSecretRedactionAudit(rec)
	req := baseDepositReq("task-1", "leaked credential in log output")
	req.Detail = "found sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR in the output"
	postBacklogDeposit(t, fx.server, req, nil)

	require.NotEmpty(t, rec.events)
	ev := rec.events[0]
	assert.Equal(t, secrets.CheckpointBacklogDeposit, ev.Checkpoint)
	assert.Equal(t, "task-1", ev.TaskID)
	assert.Equal(t, req.ProjectID, ev.ProjectID)
}

type failingSinkRecorder struct{}

func (failingSinkRecorder) Record(context.Context, []persistence.SecretRedactionEvent) error {
	return assert.AnError
}

// The best-effort contract on the API sinks (review-20260930-218b): a nil
// recorder is a no-op, and a failing record never fails the request.
func TestSecretSinks_NilAndFailingRecorderDoNotFailTheRequest(t *testing.T) {
	for name, set := range map[string]func(*Server){
		"nil":     func(*Server) {},
		"failing": func(s *Server) { s.SetSecretRedactionAudit(failingSinkRecorder{}) },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newBacklogDepositFixture(t, "", nil, nil)
			fx.server.secretsActions = map[string]secrets.Action{secrets.CheckpointBacklogDeposit: secrets.ActionRedact}
			set(fx.server)
			req := baseDepositReq("task-1", "leaked credential in log output")
			req.Detail = "found sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR in the output"
			resp := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, req, nil))
			require.Equal(t, "accepted", resp.Status, "the deposit must succeed whatever the recorder does")
		})
	}
}

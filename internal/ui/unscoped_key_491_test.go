package ui

// GitHub #491 (P1, security; operator decision B7, 2026-10-21): an unscoped
// static key that is not admin-class read the scrubbed view of every
// project's UI task, execution, replay and live pages, the log panel, and
// could fire the post-mortem explainer, because the UI chose the view from
// IsAdminClassRequest alone. Deny until scoped. Found at: review (the #440
// lane's open decision 2); an attacker holding such a key reaches it on the
// page, so the test runs the real api.AuthMiddleware with the static key
// configured and the project gate in front of the UI handler, over real
// SQLite (process-spawn-law design, "Amendment 2026-10-21 (#491)").

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/report"
)

const staticKey491UI = "sk-static-not-admin-ui-491"

type countingExplainer491 struct{ calls atomic.Int32 }

func (e *countingExplainer491) Generate(context.Context, string, bool) (*PostMortemResult, error) {
	e.calls.Add(1)
	return &PostMortemResult{}, nil
}

// asOperator491 is r with auth off (the trusted local operator). Since #491
// a request with no principal at all is refused the logs, explain and
// error text (the default arm), so a handler test that means the operator
// says so.
func asOperator491(r *http.Request) *http.Request {
	return r.WithContext(api.ContextWithAuthEnabled(r.Context(), false))
}

func (c *uiChain466) serve(t *testing.T, method, path, bearer string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, html.UnescapeString(string(b))
}

func TestTaskPage491_UnscopedStaticKeyWithheld(t *testing.T) {
	for _, scrub := range []bool{true, false} {
		exp := &countingExplainer491{}
		c := newUIChain466With(t, scrub, exp)
		for _, p := range uiPaths466 {
			code, body := c.serve(t, http.MethodGet, p, staticKey491UI)
			require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
			assertNoSecret466UI(t, p, body)
			assert.NotContains(t, body, "exit status 1", "%s (scrub=%v): a line of the error text reached an unscoped key", p, scrub)
			assert.NotContains(t, body, "step build failed", "%s (scrub=%v)", p, scrub)
			assert.NotContains(t, body, report.WithheldLine, "%s (scrub=%v): the scrubbed view reached an unscoped key", p, scrub)
			assert.Contains(t, body, api.UnscopedWithheldText, "%s (scrub=%v)", p, scrub)
		}

		code, body := c.serve(t, http.MethodGet, "/ui/tasks/t466/logs/stream", staticKey491UI)
		assert.Equal(t, http.StatusForbidden, code, "log panel (scrub=%v): %s", scrub, body)
		assert.NotContains(t, body, "exit status 1")

		code, body = c.serve(t, http.MethodPost, "/ui/tasks/t466/post-mortem", staticKey491UI)
		assert.Equal(t, http.StatusForbidden, code, "post-mortem (scrub=%v): %s", scrub, body)
		assert.Zero(t, exp.calls.Load(), "the explainer ran for an unscoped non-admin key")
	}
}

// A project-scoped key still reads its own project's scrubbed pages and may
// press the post-mortem button.
func TestTaskPage491_ScopedKeyUnchanged(t *testing.T) {
	exp := &countingExplainer491{}
	c := newUIChain466With(t, true, exp)
	for _, p := range uiPaths466 {
		body := c.get(t, p, c.projectKey)
		assert.Contains(t, body, "exit status 1", p)
		assert.NotContains(t, body, api.UnscopedWithheldText, p)
	}
	code, _ := c.serve(t, http.MethodPost, "/ui/tasks/t466/post-mortem", c.projectKey)
	assert.Equal(t, http.StatusSeeOther, code)
	assert.Equal(t, int32(1), exp.calls.Load())
}

// Adversarial review 2026-10-21 (W18-A, finding 2): the web chat's
// dispatcher tools (get_task_status, wait_for_task) put the task's raw
// last_error and error_message into the model's context and its answer is
// shown to the caller; an unscoped non-admin key got a turn on every
// project.
func TestChat491_UnscopedStaticKeyRefused(t *testing.T) {
	c := newUIChain466With(t, true, &countingExplainer491{})
	code, body := c.serve(t, http.MethodPost, "/ui/projects/alpha/chat/messages", staticKey491UI)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "PROJECT_SCOPE_REQUIRED")

	code, body = c.serve(t, http.MethodPost, "/ui/projects/alpha/chat/messages", c.projectKey)
	assert.NotEqual(t, http.StatusForbidden, code, "a project-scoped key keeps the chat: %s", body)
}

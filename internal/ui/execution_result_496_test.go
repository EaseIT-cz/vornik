package ui

// GitHub #496 (design V5, found by the local adversarial pass on the
// design): an execution's stored result is the last step's result.json,
// which on a failed step carries the step's error; the task page and the
// execution page render it (the Result panel's message and diagnostics
// error, and the raw JSON) to every caller the project gate admits, while
// #466 scrubbed the same error in error_message. A user reaches it by
// opening the page, so the test runs the #466 UI chain (the real
// api.AuthMiddleware and project gate, real SQLite).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

var resultToken496UI = "ghp_" + strings.Repeat("R4tY7uI0oP", 4)[:36]

// Code review round 1 (the local adversarial pass on 12f508fe6): the
// execution page's adaptive routing panel parsed its reason from the raw
// stored result before the page's copy was scrubbed, so a secret in
// result.reason reached every caller the project gate admits.
func TestExecutionResult496_RoutingReasonFollowsTheMapping(t *testing.T) {
	c := newUIChain466(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	result, err := json.Marshal(map[string]any{
		"status": "COMPLETED", "selected_workflow": "wf-466",
		"reason": "picked wf-466 for " + resultToken496UI + "\nrouting exit status 1",
	})
	require.NoError(t, err)
	em := errText466UI
	require.NoError(t, c.srv.execRepo.Create(ctx, &persistence.Execution{
		ID: "e496route", TaskID: "t466", ProjectID: "alpha", WorkflowID: "adaptive",
		Status: persistence.ExecutionStatusCompleted, ErrorMessage: &em, StartedAt: &now, CompletedAt: &now, Result: result,
	}))
	require.NoError(t, c.srv.execRepo.RecordCompletion(ctx, "e496route", result))

	p := "/ui/executions/e496route"
	admin := c.get(t, p, adminKey466UI)
	require.Contains(t, admin, "routing exit status 1", "the fixture must render the routing panel to the operator")
	require.Contains(t, admin, resultToken496UI)

	body := c.get(t, p, c.projectKey)
	assert.NotContains(t, body, resultToken496UI, "the routing reason served a planted secret to a project-scoped caller")
	assert.Contains(t, body, "routing exit status 1", "the routing reason's clean line was lost")

	code, body := c.serve(t, http.MethodGet, p, staticKey491UI)
	require.Equal(t, http.StatusOK, code, body)
	assert.NotContains(t, body, resultToken496UI, "the unscoped key read the routing reason")
	assert.NotContains(t, body, "routing exit status 1", "a line of the routing reason reached an unscoped key")
}

func TestExecutionResult496_PagesFollowTheMapping(t *testing.T) {
	for _, scrub := range []bool{true, false} {
		c := newUIChain466(t, scrub)
		result, err := json.Marshal(map[string]any{
			"status":      "FAILED",
			"message":     "push refused for " + resultToken496UI + "\nresult exit status 1",
			"diagnostics": map[string]any{"exitCode": 1, "error": "Authorization: Bearer " + resultToken496UI},
		})
		require.NoError(t, err)
		require.NoError(t, c.srv.execRepo.RecordCompletion(context.Background(), "e466", result))

		for _, p := range []string{"/ui/tasks/t466", "/ui/executions/e466"} {
			if scrub {
				body := c.get(t, p, c.projectKey)
				assert.NotContains(t, body, resultToken496UI, "%s: the stored result served a planted secret to a project-scoped caller", p)
				assert.Contains(t, body, "result exit status 1", "%s: the result's clean line was lost", p)
				assert.Contains(t, body, report.WithheldLine, p)
				assert.Contains(t, c.get(t, p, adminKey466UI), resultToken496UI, "%s: the operator view must stay unchanged", p)
			}
			code, body := c.serve(t, http.MethodGet, p, staticKey491UI)
			require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
			assert.NotContains(t, body, resultToken496UI, "%s (scrub=%v): the unscoped key read the stored result", p, scrub)
			assert.NotContains(t, body, "result exit status 1", "%s (scrub=%v): a line of the result reached an unscoped key", p, scrub)
			assert.Contains(t, body, api.UnscopedWithheldText, p)
		}
	}
}

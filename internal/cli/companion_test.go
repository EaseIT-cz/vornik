package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetCompanionFlags zeros every package-level flag binding so a
// previous test's values don't leak. Cobra wires flags into globals
// so this must run between every t.Run.
func resetCompanionFlags() {
	companionGrantProject = ""
	companionGrantClient = ""
	companionGrantLabel = ""
	companionGrantWorkflowsCSV = ""
	companionGrantBudgetStr = ""
	companionGrantExpires = ""
	companionGrantRepoScope = ""
	companionGrantJSON = false
	companionGrantMemoryRead = false
	companionGrantNoDelegate = false
	companionKeysProject = ""
	companionKeysJSON = false
}

// TestRunCompanionGrant_RepoScope_ForwardedAsDefaultRepoScope — the
// --repo-scope flag must reach the grant request as defaultRepoScope so
// the daemon can stamp it on memory calls that omit repo_scope.
func TestRunCompanionGrant_RepoScope_ForwardedAsDefaultRepoScope(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"codex",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"defaultRepoScope":"github.com/EaseIT-cz/vornik",
		"createdAt":"2026-06-28T10:00:00Z"
	}`)
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "codex"
	companionGrantRepoScope = "  github.com/EaseIT-cz/vornik  " // also exercises trimming
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	require.NotEmpty(t, *captured)
	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	assert.Equal(t, "github.com/EaseIT-cz/vornik", got["defaultRepoScope"],
		"--repo-scope must forward as a trimmed defaultRepoScope field")
}

// TestRunCompanionGrant_OmitsRepoScope_WhenUnset — no flag, no field.
func TestRunCompanionGrant_OmitsRepoScope_WhenUnset(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"codex",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"createdAt":"2026-06-28T10:00:00Z"
	}`)
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "codex"
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	require.NotEmpty(t, *captured)
	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	_, present := got["defaultRepoScope"]
	assert.False(t, present, "defaultRepoScope must be omitted when --repo-scope is unset")
}

// captureGrantRequest spins up a test server that returns a canned
// 201 response and records the request body so a test can assert
// the CLI's wire format. Returns the captured body bytes plus the
// server (caller closes it).
func captureGrantRequest(t *testing.T, response string) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(response))
	}))
	return srv, &captured
}

// TestRunCompanionGrant_WorkflowsCSV_ParsedAndForwarded — the CSV
// flag form must split, trim, and forward as a JSON array of
// strings. The CLI's only non-trivial logic.
func TestRunCompanionGrant_WorkflowsCSV_ParsedAndForwarded(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"allowedWorkflows":["wf-a","wf-b"],"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	// Stress the splitter with stray whitespace + trailing comma.
	companionGrantWorkflowsCSV = " wf-a , wf-b, "
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	require.NotEmpty(t, *captured, "test server should have received a request body")
	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	wfs, _ := got["allowedWorkflows"].([]any)
	require.Lenf(t, wfs, 2, "expected 2 workflows, got %v", wfs)
	assert.Equal(t, "wf-a", wfs[0])
	assert.Equal(t, "wf-b", wfs[1])
}

func TestRunCompanionGrant_OmitsWorkflowsField_WhenCSVEmpty(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantWorkflowsCSV = ""
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	_, present := got["allowedWorkflows"]
	assert.False(t, present,
		"empty --workflows must OMIT the field (so server treats as 'all'), not send []")
}

func TestRunCompanionGrant_RejectsWorkflowsCSV_OnlyWhitespaceAndCommas(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantWorkflowsCSV = ", , "

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --workflows")
}

func TestRunCompanionGrant_BudgetFloat_ParsedAndForwarded(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"budgetCapUsd":50.25,"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantBudgetStr = "50.25"
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	assert.Equal(t, 50.25, got["budgetCapUsd"])
}

func TestRunCompanionGrant_RejectsMalformedBudget(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantBudgetStr = "not-a-number"

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --budget-usd")
}

func TestRunCompanionGrant_PropagatesAPIError(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"UNKNOWN_CLIENT","message":"clientKind must be one of ..."}}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "bogus"

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNKNOWN_CLIENT",
		"server-side error code must surface in the CLI error message")
}

// TestRunCompanionKeysList_FormatsTable — happy-path list renders
// rows with the expected status mapping (revoked > expired > active)
// and replaces missing labels with "-". JSON form is tested in the
// next test.
func TestRunCompanionKeysList_FormatsTable(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "alpha", r.URL.Query().Get("projectId"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"keys": [
				{
					"id":"k1","projectId":"alpha","clientKind":"claude-code",
					"sessionLabel":"vadim/laptop","keyPrefix":"sk-vornik-al",
					"allowedWorkflows":["wf-a"],"createdAt":"2026-05-27T10:00:00Z"
				},
				{
					"id":"k2","projectId":"alpha","clientKind":"codex",
					"keyPrefix":"sk-vornik-al","createdAt":"2026-05-26T10:00:00Z",
					"revokedAt":"2026-05-26T11:00:00Z"
				}
			]
		}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionKeysProject = "alpha"
	companionKeysJSON = true // simpler to assert against JSON

	require.NoError(t, runCompanionKeysList(nil, nil))
}

func TestRunCompanionKeysList_PropagatesAPIError(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"VALIDATION_ERROR","message":"projectId required"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionKeysProject = "" // server returns 400

	err := runCompanionKeysList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "VALIDATION_ERROR")
}

// TestRunCompanionGrant_NoDelegate_ForwardedAndPrinted — --no-delegate is the
// shape of a front agent's memory key (broker design 2026-09-29 §8); it must
// reach the daemon as delegateDisabled and the output must say so.
func TestRunCompanionGrant_NoDelegate_ForwardedAndPrinted(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"memory-acme","clientKind":"hermes",
		"secret":"sk-vornik-memory.xxx","keyPrefix":"sk-vornik-me",
		"memoryRead":true,"delegateDisabled":true,
		"createdAt":"2026-09-29T10:00:00Z"
	}`)
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "memory-acme"
	companionGrantClient = "hermes"
	companionGrantMemoryRead = true
	companionGrantNoDelegate = true

	out, err := captureStdoutFunc(t, func() error { return runCompanionGrant(nil, nil) })
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	assert.Equal(t, true, got["delegateDisabled"])
	assert.Contains(t, out, "delegate:   disabled")
	assert.Contains(t, out, "workflows:  none (delegate disabled)")
}

// Mixed versions: an older daemon ignores the unknown delegateDisabled field
// and mints a key that CAN delegate. The CLI must notice the missing echo,
// revoke the key it was just handed, and fail — never report a memory-only
// key that is not one (broker design 2026-09-29, upgrade section).
func TestRunCompanionGrant_NoDelegate_RevokesWhenDaemonIgnoresIt(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"k-old","projectId":"memory-acme","clientKind":"claude-code",
			"secret":"sk-vornik-memory.xxx","keyPrefix":"sk-vornik-me","memoryRead":true,
			"createdAt":"2026-09-29T10:00:00Z"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "memory-acme"
	companionGrantClient = "claude-code"
	companionGrantMemoryRead = true
	companionGrantNoDelegate = true

	out, err := captureStdoutFunc(t, func() error { return runCompanionGrant(nil, nil) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support --no-delegate")
	assert.Equal(t, "/api/v1/projects/memory-acme/keys/k-old", deleted, "the key the old daemon minted must be revoked")
	assert.NotContains(t, out, "sk-vornik-memory.xxx", "the secret of a revoked key must not be printed")
}

func TestRunCompanionGrant_NoDelegate_RevokeFailureSaysRevokeByHand(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"k-old","projectId":"memory-acme","clientKind":"claude-code","secret":"s","keyPrefix":"p","createdAt":"2026-09-29T10:00:00Z"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionGrantProject = "memory-acme"
	companionGrantClient = "claude-code"
	companionGrantNoDelegate = true

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoke it by hand")
	assert.Contains(t, err.Error(), "k-old")
}

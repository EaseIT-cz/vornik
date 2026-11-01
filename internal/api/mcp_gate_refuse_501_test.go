package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// GitHub #501 (found at review, #498 lane W19-C). The MCP role gate admitted
// every call whose role could not be resolved, so a step whose role was
// missing, or a workflow the registry no longer held, got every
// project-enabled tool instead of none. A gap that means "this task's role
// could not be resolved" now refuses; the deliberate open gaps (no task id,
// nothing wired, a role declaring no tools, a finished execution) stay open
// and counted. Integration level: a real registry loaded from files and the
// real /mcp/call and ListMCPTools handlers.

func TestServer_CallMCPTool_UnresolvedStepIsRefused_501(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "should-not-run"}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-x", "no-such-step")
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	server.mcpGateMetrics = m

	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, taskScopedMCPRequest("task-x", `{"name":"mcp__broker__place_order"}`))

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "step_not_found")
	require.Empty(t, f.lastTool, "the executor must not be reached when the role cannot be resolved")
	require.Equal(t, 1.0, refusedValue(t, m, "call", string(mcpGapStepNotFound)))
	require.Equal(t, 0.0, counterValue(t, m, "call", string(mcpGapStepNotFound)),
		"a refused gap is not an unrestricted one")
}

// A finished execution stays open: on 2026-09-04 every fail-open measured was
// an authenticated call for an execution parked at adaptive's terminals.
func TestServer_CallMCPTool_TerminalExecutionStaysOpen_501(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "ok"}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-x", "done")

	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, taskScopedMCPRequest("task-x", `{"name":"mcp__broker__place_order"}`))

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestGapRefuses_501(t *testing.T) {
	refuse := map[mcpGapReason]bool{
		mcpGapNoExecution: true, mcpGapNoWorkflow: true, mcpGapStepNotFound: true,
		mcpGapNoStepRole: true, mcpGapNoSwarm: true, mcpGapRoleNotFound: true,
	}
	for _, r := range allMCPGapReasons() {
		if gapRefuses(r) != refuse[r] {
			t.Errorf("gapRefuses(%s) = %v, want %v", r, gapRefuses(r), refuse[r])
		}
	}
	if gapRefuses(mcpGapNone) {
		t.Error("a resolved allowlist is not a gap")
	}
}

// The advertised list agrees with the call path: a refusing gap advertises
// no MCP tools, so the agent is not offered tools it will be refused.
func TestServer_ListMCPTools_UnresolvedStepAdvertisesNone_501(t *testing.T) {
	f := &fakeMCPExecutor{tools: map[string][]chat.Tool{"project-1": {
		{Type: "function", Function: chat.ToolFunction{Name: "mcp__broker__get_quote"}},
	}}}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-x", "no-such-step")

	req := taskScopedMCPRequest("task-x", "")
	req.Method = http.MethodGet
	req.URL.Path = "/api/v1/projects/project-1/mcp/tools"
	rec := httptest.NewRecorder()
	server.ListMCPTools(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Tools []chat.Tool `json:"tools"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Empty(t, resp.Tools)
}

func refusedValue(t *testing.T, m *MCPGateMetrics, path, reason string) float64 {
	t.Helper()
	return counterVecValue(t, m.RefusedTotal, path, reason)
}

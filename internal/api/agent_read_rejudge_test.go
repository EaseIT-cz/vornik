package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #413 (2026-10-17): an approved read tool was never re-judged. The
// MCP manager refuses an agent read call whose tool the server now describes
// as changing things (agent-administered design §6.5); the handler answers
// that refusal as the approval gate does, 403, not as a server fault (502).
func TestCallMCPTool_AgentReadRejudgeIsForbidden(t *testing.T) {
	exec := &mockMCPExecutor{executed: func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("MCP tool mcp__mail__search_messages failed: %w", mcp.ErrReadToolChangesThings)
	}}
	srv := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(exec), WithAgentGrants(memGrants{rows: map[string]persistence.AgentIntegrationApproval{
		"hermes--comms/mail": {ReadTools: []string{"search_messages"}, WriteTools: []string{}},
	}}))
	srv.agentRoleAllowlistForTest = func(context.Context, string) ([]string, mcpGapReason) {
		return []string{"mcp__mail__search_messages"}, ""
	}
	body := `{"name":"mcp__mail__search_messages","arguments":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/hermes--comms/mcp/tools/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Task-ID", "t1")
	req.SetPathValue("id", "hermes--comms")
	rec := httptest.NewRecorder()
	srv.CallMCPTool(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "changes things") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if srv.agentToolCounts.Refused.Load() != 1 {
		t.Fatalf("the refusal is not counted: %d", srv.agentToolCounts.Refused.Load())
	}
}

func TestDoctor_AgentReadRejudge(t *testing.T) {
	h := &DoctorHandlers{}
	if c := h.checkAgentReadRejudge(); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", c)
	}
	h.SetAgentReadVerdicts(func() ([]mcp.ReadEntryVerdict, []string) {
		return []mcp.ReadEntryVerdict{{ProjectID: "hermes--comms", Server: "mail", Examined: []string{"a", "b"}}}, []string{"hermes--cal/cal"}
	})
	c := h.checkAgentReadRejudge()
	if c.Status != "OK" || !strings.Contains(c.Message, "2 approved read tool(s) on 1 connected agent server(s)") ||
		!strings.Contains(c.Message, "1 agent server(s) not connected, not examined (hermes--cal/cal)") {
		t.Fatalf("clean: %+v", c)
	}
	h.SetAgentReadVerdicts(func() ([]mcp.ReadEntryVerdict, []string) {
		return []mcp.ReadEntryVerdict{{ProjectID: "hermes--comms", Server: "mail", Examined: []string{"a", "b"}, Suspended: []string{"b"}, Unlisted: []string{"c"}}}, nil
	})
	c = h.checkAgentReadRejudge()
	if c.Status != "WARNING" || !strings.Contains(c.Message, "1 suspended; 1 no longer listed") ||
		!strings.Contains(c.Message, "hermes--comms/mail suspended: b") || !strings.Contains(c.Message, "hermes--comms/mail no longer listed: c") {
		t.Fatalf("suspended: %+v", c)
	}
}

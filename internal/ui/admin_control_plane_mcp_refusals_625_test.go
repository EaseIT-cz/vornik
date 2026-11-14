package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/mcp"
)

// GitHub #625 item 5 (agent-administered design, "Follow-up 2026-11-12 (#625
// item 5)"): the hub's MCP section lists every server refused at registration,
// with its scope and reason; before, the reason was only in the journal.
// Found at: review (#623's "Not in this change"). Regression test at:
// integration (the production hub handler).

func TestMCPHubRefusals625_Listed(t *testing.T) {
	s, _ := mcpTestServer(t)
	WithMCPRefusals(func() []mcp.Refusal {
		return []mcp.Refusal{
			{Scope: "", Server: "n8n", Reason: "secret://N8N_TOKEN is not set"},
			{Scope: "assistant", Server: "homeassistant", Reason: "subscribed by name only, but config.yaml mcp.servers does not define it"},
		}
	})(s)
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, setupAuthRequest(http.MethodGet, "/admin/control-plane?section=mcp", "admin"))
	body := rec.Body.String()
	for _, want := range []string{
		`data-mcp-refused`,
		`data-refused="daemon/n8n"`, "secret://N8N_TOKEN is not set",
		`data-refused="assistant/homeassistant"`, "does not define it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hub MCP section lacks %q (status %d)", want, rec.Code)
		}
	}
}

func TestMCPHubRefusals625_NoBlockWhenNone(t *testing.T) {
	s, _ := mcpTestServer(t)
	WithMCPRefusals(func() []mcp.Refusal { return nil })(s)
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, setupAuthRequest(http.MethodGet, "/admin/control-plane?section=mcp", "admin"))
	if strings.Contains(rec.Body.String(), "data-mcp-refused") {
		t.Fatal("an empty refusal list rendered the block")
	}
}

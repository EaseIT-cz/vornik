package ui

import (
	"strings"
	"testing"
)

// GitHub #101 (2026-10-08): the assistants page shows, per connection, when
// it was last tested with test_connection and the result, or that it never
// was; a stored credential is not shown as accepted.
func TestAssistantsPage_ShowsLastCheckedPerConnection(t *testing.T) {
	page := &AgentPage{
		Row: AgentRow{Namespace: "hermes", ClientKind: "hermes", Class: "mcp_only", KeyStatus: "active"},
		Projects: []AgentProject{{ID: "hermes--finance", Purpose: "Money",
			Integrations: []AgentItem{
				{Name: "fio", Status: "approved", Detail: "https://api.fio.example/v1", Checked: "last tested 2026-10-08 09:30 UTC: auth_rejected"},
				{Name: "bank", Status: "approved", Detail: "https://bank.example/mcp", Checked: "not tested"},
			},
			Credentials: []AgentItem{{Name: "FIO", Status: "stored"}}}},
	}
	srv := NewServer(WithAgents(stubAgents{rows: []AgentRow{page.Row}, page: page}))
	body := getAgents(t, srv, "/admin/agents/hermes", true).Body.String()
	for _, want := range []string{"last tested 2026-10-08 09:30 UTC: auth_rejected", "not tested", "stored"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

package ui

import (
	"strings"
	"testing"
)

// GitHub #172 (2026-10-13): refused agent-admin calls were invisible to the
// operator. The list shows a 7-day count per assistant; the assistant's page
// shows the count by rule, the recent rows, what was counted but not
// written, and states what is and is not recorded (design §23.2).
func TestAdminAgents_Refusals(t *testing.T) {
	page := &AgentPage{
		Row: AgentRow{Namespace: "hermes", ClientKind: "hermes", Class: "mcp_only", KeyStatus: "active", KeyPrefix: "sk-vornik-hermes--h.ab", Refused7d: 4},
		Refusals: AgentRefusals{Total: 4, Unwritten: 2,
			ByRule: []AgentRuleCount{{Rule: "out_of_namespace", Count: 3}, {Rule: "other", Count: 1}},
			Recent: []AgentRefusalRow{{When: "2026-10-13 12:00 UTC", Verb: "define_workflow", Rule: "out_of_namespace", KeyID: "akey-hermes-1"}}},
	}
	srv := NewServer(WithAgents(stubAgents{rows: []AgentRow{page.Row}, page: page}))

	list := getAgents(t, srv, "/admin/agents", true).Body.String()
	if !strings.Contains(list, "Refused (7 days)") || !strings.Contains(list, ">4<") {
		t.Fatalf("the list does not show the 7-day refused count:\n%s", list)
	}
	body := getAgents(t, srv, "/admin/agents/hermes", true).Body.String()
	for _, want := range []string{"Refused calls", "out_of_namespace", "3", "other", "define_workflow", "akey-hermes-1",
		"2 more refusal", "Recorded:", "Not recorded:", "never the input", "show_workflow", "outside Vornik"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	// None recorded: the section still states what is recorded, so an empty
	// list reads as "examined and clean", not as "never examined".
	page.Refusals = AgentRefusals{}
	body = getAgents(t, srv, "/admin/agents/hermes", true).Body.String()
	for _, want := range []string{"Refused calls", "No refusal in the last 7 days", "Not recorded:"} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty section does not show %q", want)
		}
	}
}

// #334 audit 2026-10-14: a failed refusal read used to fail the whole page.
// It now says "refusals unavailable" in the list and in the section, and
// never reads as "No refusal in the last 7 days".
func TestAdminAgents_RefusalsUnavailable(t *testing.T) {
	page := &AgentPage{
		Row:      AgentRow{Namespace: "hermes", ClientKind: "hermes", Class: "mcp_only", KeyStatus: "active", KeyPrefix: "sk-vornik-hermes--h.ab", RefusedUnavailable: true},
		Refusals: AgentRefusals{Unavailable: true},
	}
	srv := NewServer(WithAgents(stubAgents{rows: []AgentRow{page.Row}, page: page}))
	list := getAgents(t, srv, "/admin/agents", true).Body.String()
	if !strings.Contains(list, "refusals unavailable") {
		t.Fatalf("the list does not mark the count unavailable:\n%s", list)
	}
	body := getAgents(t, srv, "/admin/agents/hermes", true).Body.String()
	if !strings.Contains(body, "Refusals unavailable") || strings.Contains(body, "No refusal in the last 7 days") {
		t.Fatalf("the section must say unavailable, not none:\n%s", body)
	}
}

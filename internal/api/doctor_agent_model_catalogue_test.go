package api

import (
	"strings"
	"testing"
)

// agent_model_catalogue (agent-administered design §18.6 item 2, Change 2;
// round 2 F5; round 3 F1): every agent_admin.models entry that is not offered
// (no route, a command-line provider, remote without an exact price) is a
// doctor finding, and the check states how many entries it examined, so OK
// over an empty catalogue reads as what it is.
func TestCheckAgentModelCatalogue(t *testing.T) {
	cases := []struct {
		name     string
		src      func() AgentModelCatalogueStatus
		status   string
		mentions []string
	}{
		{"not wired", nil, "SKIPPED", []string{"not wired"}},
		{"empty catalogue", func() AgentModelCatalogueStatus { return AgentModelCatalogueStatus{} }, "OK",
			[]string{"0 catalogue entr", "no model is offered"}},
		{"all offered", func() AgentModelCatalogueStatus {
			return AgentModelCatalogueStatus{Examined: 2, Offered: 2}
		}, "OK", []string{"2 catalogue entr", "2 offered"}},
		{"some dropped", func() AgentModelCatalogueStatus {
			return AgentModelCatalogueStatus{Examined: 3, Offered: 1, Dropped: []AgentModelFinding{
				{ID: "claude-sonnet", Why: "a command-line provider"},
				{ID: "unpriced", Why: "pricing.yaml has no exact entry"},
			}}
		}, "WARNING", []string{"3 catalogue entr", "1 offered", "claude-sonnet", "command-line", "unpriced", "pricing.yaml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.src != nil {
				h.SetAgentModelCatalogue(c.src)
			}
			got := h.checkAgentModelCatalogue()
			if got.Name != "agent_model_catalogue" || got.Status != c.status {
				t.Fatalf("got %s %s %q, want %s", got.Name, got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Errorf("message %q does not mention %q", got.Message, m)
				}
			}
		})
	}
}

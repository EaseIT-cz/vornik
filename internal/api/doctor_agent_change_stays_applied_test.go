package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// GitHub #373 (agent-administered design §23.9): an agent change whose
// rollback failed stayed applied with nothing the doctor could see.
// agent_change_stays_applied names each one still APPLIED and always states
// how many it examined, so an OK over none reads as vacuous.
func TestCheckAgentChangeStaysApplied(t *testing.T) {
	cases := []struct {
		name   string
		rep    AgentRollbackFailures
		err    error
		status string
		want   []string
		absent []string
	}{
		{"none", AgentRollbackFailures{}, nil, "OK",
			[]string{"0 failed agent-change rollback(s) examined in the last 30 days", "0 still applied"}, nil},
		{"resolved", AgentRollbackFailures{Examined: 2, Resolved: 2}, nil, "OK",
			[]string{"2 failed agent-change rollback(s) examined", "0 still applied, 2 resolved"}, nil},
		{"stays applied", AgentRollbackFailures{Examined: 3, Resolved: 2,
			StillApplied: []StaysAppliedChange{{ProposalID: "cpp_1", Namespace: "hermes", Verb: "remove", Stage: "record"}}}, nil, "WARNING",
			[]string{"1 still applied", "cpp_1 (hermes, remove, record)", "vornikctl control-plane rollback <id>"}, nil},
		{"unknown", AgentRollbackFailures{Examined: 1, Unknown: []string{"cpp_2"}}, nil, "WARNING",
			[]string{"1 unreadable", "cpp_2"}, nil},
		{"full page", AgentRollbackFailures{Examined: 100, Resolved: 100, Full: true}, nil, "OK",
			[]string{"at least 100"}, nil},
		{"read error", AgentRollbackFailures{}, errors.New("db down"), "WARNING", []string{"could not read"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			h.SetAgentRollbackFailures(func(context.Context) (AgentRollbackFailures, error) { return c.rep, c.err })
			got := h.checkAgentChangeStaysApplied(context.Background())
			if got.Name != "agent_change_stays_applied" || got.Status != c.status {
				t.Fatalf("%+v, want status %s", got, c.status)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Message, w) {
					t.Errorf("message %q lacks %q", got.Message, w)
				}
			}
		})
	}
	if got := (&DoctorHandlers{}).checkAgentChangeStaysApplied(context.Background()); got.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", got)
	}
}

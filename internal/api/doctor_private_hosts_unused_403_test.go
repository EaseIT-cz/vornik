package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// agent_private_hosts_unused (agent-administered design part 06 §6.12 item 8,
// GitHub #403): an agent_admin.private_api_hosts entry no approved agent
// connection uses is reach nobody needs, which an agent can later ask for.
// The check states its denominator: entries examined against connections.
func TestCheckAgentPrivateHostsUnused_403(t *testing.T) {
	cases := []struct {
		name     string
		src      func(context.Context) (AgentPrivateHostsStatus, error)
		status   string
		mentions []string
	}{
		{"not wired", nil, "SKIPPED", []string{"not wired"}},
		{"no entries", func(context.Context) (AgentPrivateHostsStatus, error) {
			return AgentPrivateHostsStatus{Connections: 4}, nil
		}, "SKIPPED", []string{"agent_admin.private_api_hosts is empty"}},
		{"read error", func(context.Context) (AgentPrivateHostsStatus, error) {
			return AgentPrivateHostsStatus{}, errors.New("db down")
		}, "WARNING", []string{"could not read", "db down"}},
		{"all used", func(context.Context) (AgentPrivateHostsStatus, error) {
			return AgentPrivateHostsStatus{Entries: 2, Connections: 3}, nil
		}, "OK", []string{"2 entries examined against 3 approved agent connection(s)", "each grants at least one"}},
		{"unused", func(context.Context) (AgentPrivateHostsStatus, error) {
			return AgentPrivateHostsStatus{Entries: 3, Connections: 1, Unused: []string{"nas.lan:8443", "wiki.lan within 10.0.0.0/24 (projects hermes--home)"}}, nil
		}, "WARNING", []string{"3 entries examined against 1 approved agent connection(s)", "2 grant no connection",
			"nas.lan:8443", "wiki.lan within 10.0.0.0/24 (projects hermes--home)", "Remove an entry nothing needs"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.src != nil {
				h.SetAgentPrivateHostsUnused(c.src)
			}
			got := h.checkAgentPrivateHostsUnused(context.Background())
			if got.Name != "agent_private_hosts_unused" || got.Status != c.status {
				t.Fatalf("got %s %s (%s), want agent_private_hosts_unused %s", got.Name, got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Errorf("message lacks %q: %s", m, got.Message)
				}
			}
		})
	}
}

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// agent_connections_cgnat (agent-administered design part 06 §6.11, GitHub
// #893): approved agent connections whose host resolves to CGNAT
// 100.64.0.0/10. Since #893 option A the dial guard refuses that range
// unless the host is on agent_admin.private_api_hosts, so an unlisted one is
// an ERROR (its every dial is refused) and a listed one is OK. The check
// states how many connections it examined and how many it could not
// resolve, so OK reads as "examined and clean", never "not looked".
func TestCheckAgentCGNATConnections_893(t *testing.T) {
	cases := []struct {
		name     string
		src      func(context.Context) (AgentCGNATStatus, error)
		status   string
		mentions []string
	}{
		{"not wired", nil, "SKIPPED", []string{"not wired"}},
		{"read error", func(context.Context) (AgentCGNATStatus, error) {
			return AgentCGNATStatus{}, errors.New("db down")
		}, "WARNING", []string{"could not read", "db down"}},
		{"none", func(context.Context) (AgentCGNATStatus, error) {
			return AgentCGNATStatus{}, nil
		}, "OK", []string{"0 approved agent connection(s) examined", "none reaches"}},
		{"clean with an unresolved host", func(context.Context) (AgentCGNATStatus, error) {
			return AgentCGNATStatus{Examined: 3, Resolved: 2, Unresolved: []string{"hermes/hermes-mail/imap (mail.example)"}}, nil
		}, "OK", []string{"3 approved agent connection(s) examined", "2 resolved", "1 could not be resolved", "hermes/hermes-mail/imap (mail.example)", "none reaches"}},
		{"unlisted", func(context.Context) (AgentCGNATStatus, error) {
			return AgentCGNATStatus{Examined: 2, Resolved: 2, Found: []AgentCGNATConnection{
				{Namespace: "hermes", ProjectID: "hermes-home", Integration: "nas", Host: "nas.tail1234.ts.net", Addrs: []string{"100.101.1.2"}},
				{Namespace: "hermes", ProjectID: "hermes-home", Integration: "ha", Host: "ha.lan.example", Addrs: []string{"100.64.0.9"}, Listed: true},
			}}, nil
		}, "ERROR", []string{"2 approved agent connection(s) examined", "2 reach", "100.64.0.0/10",
			"1 on agent_admin.private_api_hosts",
			"1 not on the list",
			"hermes/hermes-home/nas nas.tail1234.ts.net -> 100.101.1.2",
			"refused at every dial",
			"https", "for an IP-literal URL, the address"}},
		{"all listed", func(context.Context) (AgentCGNATStatus, error) {
			return AgentCGNATStatus{Examined: 1, Resolved: 1, Found: []AgentCGNATConnection{
				{Namespace: "hermes", ProjectID: "hermes-home", Integration: "ha", Host: "ha.lan.example", Addrs: []string{"100.64.0.9"}, Listed: true},
			}}, nil
		}, "OK", []string{"1 approved agent connection(s) examined", "1 reach", "all on agent_admin.private_api_hosts",
			"hermes/hermes-home/ha ha.lan.example -> 100.64.0.9"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.src != nil {
				h.SetAgentCGNATConnections(c.src)
			}
			got := h.checkAgentCGNATConnections(context.Background())
			if got.Name != "agent_connections_cgnat" || got.Status != c.status {
				t.Fatalf("got %s %s (%s), want agent_connections_cgnat %s", got.Name, got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Errorf("message %q lacks %q", got.Message, m)
				}
			}
		})
	}
}

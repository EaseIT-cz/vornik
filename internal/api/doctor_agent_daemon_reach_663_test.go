package api

import (
	"strings"
	"testing"
)

// GitHub #663 (onboarding-hardening design D6). Found at: e2e-lane (a bench
// daemon bound to 127.0.0.1): every agent's curl to the daemon was refused,
// the circuit opened, and nothing pointed at server.address. Regression test
// at: unit, the check's decision; lower because the e2e lane needs a
// container host. The check reads every role's effective network.
func TestAgentDaemonReach663(t *testing.T) {
	cases := []struct {
		name, listen, defaultNet string
		roles                    []string
		status, want             string
	}{
		{"loopback, one TCP role", "127.0.0.1:8091", "", []string{""}, "ERROR", "1 role"},
		{"localhost, TCP roles", "localhost:8080", "", []string{"", "host"}, "ERROR", "2 role"},
		{"ipv6 loopback", "[::1]:8080", "", []string{""}, "ERROR", "1 role"},
		{"all interfaces", "0.0.0.0:8080", "", []string{""}, "OK", "not loopback"},
		{"empty host", ":8080", "", []string{""}, "OK", "not loopback"},
		{"loopback, default daemon-only", "127.0.0.1:8080", "daemon-only", []string{"", ""}, "OK", "daemon-only"},
		{"loopback, every role daemon-only", "127.0.0.1:8080", "", []string{"daemon-only"}, "OK", "daemon-only"},
		{"loopback, default daemon-only, one role host", "127.0.0.1:8080", "daemon-only", []string{"", "host"}, "ERROR", "1 role"},
		{"loopback, no roles yet", "127.0.0.1:8080", "", nil, "OK", "no swarm role"},
	}
	for _, c := range cases {
		got := agentDaemonReach(c.listen, c.defaultNet, c.roles)
		if got.Name != "agent_daemon_reach" || got.Status != c.status || !strings.Contains(got.Message, c.want) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, got.Status, got.Message, c.status, c.want)
		}
		if got.Status == "ERROR" && !strings.Contains(got.Message, "0.0.0.0") {
			t.Errorf("%s: the error must name the fix: %q", c.name, got.Message)
		}
	}
}

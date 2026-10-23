package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// GitHub #465 (2026-10-21): server.accepted_hosts is the operator-facing list
// of extra Host names the TCP listener answers when api.auth_enabled is false
// (the DNS-rebinding guard, cloudflare-real-ip-design "Amendment 2026-10-21
// (#465)"). Its entries are validated where the guard is built
// (service.tcpRootHandler), not here.
func TestConfig_ServerAcceptedHostsParses_Issue465(t *testing.T) {
	const y = `
server:
  accepted_hosts: ["vornik.lan", "Desk.Example.Test"]
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.Server.AcceptedHosts; len(got) != 2 || got[0] != "vornik.lan" || got[1] != "Desk.Example.Test" {
		t.Fatalf("accepted_hosts = %v", got)
	}
}

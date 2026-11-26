package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// GitHub #403 (agent-administered design part 06, §6.12): an
// agent_admin.private_api_hosts item is a string, as before, or a mapping
// with host, addresses (pins) and projects. Host may be an IP literal or
// host:port.

func loadAgentAdmin403(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	c := baseValidConfig()
	if err := yaml.Unmarshal([]byte(doc), &c.AgentAdmin); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func TestAgentAdminPrivateAPIHosts_403_BothForms(t *testing.T) {
	c, err := loadAgentAdmin403(t, `
private_api_hosts:
  - GitLab.Lan.
  - nas.lan:8443
  - 10.0.0.5
  - "[fd00::5]:8443"
  - host: wiki.lan
    addresses: [10.0.0.0/24, 10.0.1.7]
    projects: [hermes--homelab]
`)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	got := c.AgentAdmin.PrivateHosts()
	want := []string{"gitlab.lan", "nas.lan:8443", "10.0.0.5", "[fd00::5]:8443", "wiki.lan within 10.0.0.0/24, 10.0.1.7"}
	if len(got) != len(want) {
		t.Fatalf("entries %v", got)
	}
	for i, w := range want {
		if got[i].String() != w {
			t.Errorf("entry %d = %q, want %q", i, got[i].String(), w)
		}
	}
	if len(got[4].Projects) != 1 || got[4].Projects[0] != "hermes--homelab" || len(got[0].Projects) != 0 {
		t.Errorf("projects: %v / %v", got[4].Projects, got[0].Projects)
	}
	// The plain form is folded in place, as before #403.
	if c.AgentAdmin.PrivateAPIHosts[0].Host != "gitlab.lan" {
		t.Errorf("host not folded: %q", c.AgentAdmin.PrivateAPIHosts[0].Host)
	}
}

func TestAgentAdminPrivateAPIHosts_403_Refusals(t *testing.T) {
	for name, doc := range map[string]string{
		"loopback literal":   "private_api_hosts: [127.0.0.1]",
		"link-local literal": "private_api_hosts: [169.254.10.1]",
		"metadata literal":   "private_api_hosts: ['fd00:ec2::254']",
		"NAT64 literal":      "private_api_hosts: ['64:ff9b::a00:5']",
		"bad port":           "private_api_hosts: [nas.lan:70000]",
		"wildcard":           "private_api_hosts: ['*.lan']",
		"unknown key":        "private_api_hosts: [{host: nas.lan, ports: [443]}]",
		"no host":            "private_api_hosts: [{projects: [hermes--home]}]",
		"operator project":   "private_api_hosts: [{host: nas.lan, projects: [family]}]",
		"pin on a literal":   "private_api_hosts: [{host: 10.0.0.5, addresses: [10.0.0.0/24]}]",
		"loopback pin":       "private_api_hosts: [{host: nas.lan, addresses: [127.0.0.0/8]}]",
		"bad pin":            "private_api_hosts: [{host: nas.lan, addresses: [nope]}]",
	} {
		if _, err := loadAgentAdmin403(t, doc); err == nil || !strings.Contains(err.Error(), "agent_admin.private_api_hosts") {
			t.Errorf("%s: err = %v, want a refusal naming agent_admin.private_api_hosts", name, err)
		}
	}
}

// A list valid before #403 loads with the same meaning: every agent project,
// by name.
func TestAgentAdminPrivateAPIHosts_403_PlainListUnchanged(t *testing.T) {
	c, err := loadAgentAdmin403(t, "private_api_hosts: [gitlab.corp.example, wiki.corp.example]")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.AgentAdmin.PrivateHosts() {
		if e.Port != "" || len(e.Addresses) != 0 || len(e.Projects) != 0 {
			t.Errorf("plain entry %+v gained a scope", e)
		}
	}
}

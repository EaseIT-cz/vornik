package config

import (
	"reflect"
	"strings"
	"testing"
)

// GitHub #734 (agent-administered design part 06, §6.7): an operator names the
// intranet hosts an agent project's approved API may reach on a private
// address. The list is bare host names, folded at load, and self-hosted only.
func TestAgentAdminPrivateAPIHosts_734(t *testing.T) {
	c := baseValidConfig()
	c.AgentAdmin.PrivateAPIHosts = []string{"GitLab.Corp.Example.", "wiki.corp.example"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid hosts refused: %v", err)
	}
	if want := []string{"gitlab.corp.example", "wiki.corp.example"}; !reflect.DeepEqual(c.AgentAdmin.PrivateAPIHosts, want) {
		t.Fatalf("hosts = %q, want folded %q", c.AgentAdmin.PrivateAPIHosts, want)
	}
	for _, bad := range []string{"10.0.0.5", "gitlab.corp.example:443", "https://gitlab.corp.example", "*.corp.example", "gitlab.corp.example/api", ""} {
		c := baseValidConfig()
		c.AgentAdmin.PrivateAPIHosts = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "agent_admin.private_api_hosts") {
			t.Errorf("%q: err = %v, want a refusal naming agent_admin.private_api_hosts", bad, err)
		}
	}
	m := managedConfig()
	m.AgentAdmin.PrivateAPIHosts = []string{"gitlab.corp.example"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "agent_admin.private_api_hosts") {
		t.Errorf("managed hosting: err = %v, want the setting refused", err)
	}
}

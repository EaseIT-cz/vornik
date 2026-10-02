package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// Agent-administered Vornik plan P6.4: agent_admin is on unless a config
// says enabled: false. Nothing is reachable without an operator-minted agent
// admin key, and minting needs a paired approver device. Control: IsEnabled.
func TestAgentAdmin_DefaultsOn(t *testing.T) {
	var unset AgentAdminConfig
	if !unset.IsEnabled() {
		t.Fatal("an unset agent_admin is off")
	}
	for doc, want := range map[string]bool{"enabled: false": false, "enabled: true": true, "default_project_budget_usd: 3": true} {
		var a AgentAdminConfig
		if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
			t.Fatal(err)
		}
		if a.IsEnabled() != want {
			t.Errorf("%q: enabled = %v, want %v", doc, a.IsEnabled(), want)
		}
	}
}

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// an agent_admin.agent_image written before the move names the legacy
// repository; agents' roles render with the canonical name.
func TestAgentAdmin_LegacyAgentImageIsCanonical(t *testing.T) {
	a := AgentAdminConfig{AgentImage: "ghcr.io/grinco/vornik-agent:2026.9.8"}
	if got := a.EffectiveAgentImage("ghcr.io/easeit-cz/vornik-agent:latest"); got != "ghcr.io/easeit-cz/vornik-agent:2026.9.8" {
		t.Fatalf("EffectiveAgentImage = %q", got)
	}
	if got := (AgentAdminConfig{AgentImage: "localhost/mine:1"}).EffectiveAgentImage("x"); got != "localhost/mine:1" {
		t.Fatalf("an unrelated configured image changed: %q", got)
	}
}

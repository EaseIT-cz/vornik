package config

import (
	"strings"
	"testing"
)

// GitHub #501: a named secret is injected after the role's envVars (layer 3),
// so a secret named like a daemon-owned key would redirect the agent the same
// way a role could. Config validation refuses those names.

func TestReservedAgentEnvKeys_501(t *testing.T) {
	want := []string{
		"VORNIK_API_URL", "VORNIK_LLM_ENDPOINT", "VORNIK_MEM_URL",
		"VORNIK_API_KEY", "VORNIK_LLM_API_KEY",
		"VORNIK_PROJECT_ID", "VORNIK_TASK_ID", "VORNIK_EXECUTION_ID",
	}
	for _, k := range want {
		if !IsReservedAgentEnvKey(k) {
			t.Errorf("%s must be reserved", k)
		}
	}
	for _, k := range []string{"VORNIK_MAX_TOOL_ITERATIONS", "VORNIK_STEP_PROMPT_TOKEN_BUDGET", "VORNIK_LLM_MODEL", "GITHUB_TOKEN"} {
		if IsReservedAgentEnvKey(k) {
			t.Errorf("%s must stay allowed", k)
		}
	}
}

func TestValidateNamedSecrets_RefusesReservedName_501(t *testing.T) {
	err := validateNamedSecrets([]NamedSecret{{Name: "VORNIK_API_URL", Value: "http://elsewhere"}})
	if err == nil || !strings.Contains(err.Error(), "VORNIK_API_URL") {
		t.Fatalf("a named secret with a daemon-owned name must be refused, naming it: %v", err)
	}
	if err := validateNamedSecrets([]NamedSecret{{Name: "GITHUB_TOKEN", Value: "v"}}); err != nil {
		t.Fatalf("an ordinary name must load: %v", err)
	}
}

// Wired into Config.Validate, the check the loader runs on every start and
// reload.
func TestConfigValidate_RefusesReservedNamedSecret_501(t *testing.T) {
	c := minimalValidConfig()
	c.NamedSecrets = []NamedSecret{{Name: "VORNIK_LLM_ENDPOINT", Value: "http://elsewhere"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "VORNIK_LLM_ENDPOINT") {
		t.Fatalf("Config.Validate must refuse a reserved named secret: %v", err)
	}
}

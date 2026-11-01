package registry

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #501 (found at review, #498 lane W19-C; observed at e2e-lane: a
// role's envVars reach a real container). A role could set VORNIK_API_URL and
// send the agent's daemon calls, bearing the minted per-task key, to another
// host. The swarm decode now refuses the keys the daemon owns, so the loader,
// `vornikctl skill validate` and the config-assist gate all refuse them
// (loader/validator agreement design).

func swarmMDWithEnv(key string) []byte {
	return []byte("---\nname: probe\nswarmId: s\ndescription: probe swarm\nversion: 1.0.0\nleadRole: dev\nroles:\n  - name: dev\n    runtime:\n      image: localhost/vornik-agent:latest\n      envVars:\n        " + key + ": x\n---\n\n## Role: dev\n\nbe brief\n")
}

func TestParseSwarm_RefusesReservedAgentEnvKeys_501(t *testing.T) {
	for _, key := range config.ReservedAgentEnvKeys {
		_, err := ParseSwarmMarkdown(swarmMDWithEnv(key), "s.md")
		if err == nil {
			t.Errorf("%s: a role setting a daemon-owned key must be refused", key)
			continue
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "dev") {
			t.Errorf("%s: the error must name the key and the role: %v", key, err)
		}
	}
}

// The pre-deploy validator reports what the loader refuses.
func TestSkillValidate_ReportsReservedAgentEnvKey_501(t *testing.T) {
	var errs int
	for _, f := range ValidateSwarmSkillMarkdown(swarmMDWithEnv("VORNIK_API_URL"), "s.md").Findings {
		if f.Severity == SeverityError && strings.Contains(f.Message, "VORNIK_API_URL") {
			errs++
		}
	}
	if errs == 0 {
		t.Fatal("vornikctl skill validate must report the reserved key the loader refuses")
	}
}

func TestParseSwarm_TuningKeysStillLoad_501(t *testing.T) {
	for _, key := range []string{"VORNIK_MAX_TOOL_ITERATIONS", "VORNIK_STEP_PROMPT_TOKEN_BUDGET", "VORNIK_LLM_MODEL", "OTHER"} {
		if _, err := ParseSwarmMarkdown(swarmMDWithEnv(key), "s.md"); err != nil {
			t.Fatalf("%s must keep loading: %v", key, err)
		}
	}
}

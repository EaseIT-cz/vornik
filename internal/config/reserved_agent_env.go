package config

import (
	"fmt"
	"slices"
)

// ReservedAgentEnvKeys are the agent container environment keys the daemon
// owns: where the agent's daemon calls go, the credentials they carry, and
// the task's identity. A role's envVars and a named secret are layered over
// the daemon's own values, so either could otherwise redirect the agent's
// calls, bearing the minted per-task key, to another host (GitHub #501;
// agent runtime contract §14). Tuning keys stay settable.
var ReservedAgentEnvKeys = []string{
	"VORNIK_API_URL", "VORNIK_LLM_ENDPOINT", "VORNIK_MEM_URL",
	"VORNIK_API_KEY", "VORNIK_LLM_API_KEY",
	"VORNIK_PROJECT_ID", "VORNIK_TASK_ID", "VORNIK_EXECUTION_ID",
}

// IsReservedAgentEnvKey reports whether key is one the daemon owns.
func IsReservedAgentEnvKey(key string) bool {
	return slices.Contains(ReservedAgentEnvKeys, key)
}

// validateNamedSecrets refuses a named secret whose name the daemon owns.
func validateNamedSecrets(secrets []NamedSecret) error {
	for _, s := range secrets {
		if IsReservedAgentEnvKey(s.Name) {
			return fmt.Errorf("named_secrets: %q is set by the daemon for every agent container and cannot be a named secret", s.Name)
		}
	}
	return nil
}

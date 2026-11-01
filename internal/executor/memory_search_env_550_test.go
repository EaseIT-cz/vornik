package executor

// GitHub #550. Design: https://docs.vornik.io
// §2.1 C1. A role's memorySearch policy reaches its container as environment,
// so handle_memory_search sends it without the model asking; the daemon
// re-applies it server-side either way.

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestApplyMemorySearchPolicyEnv(t *testing.T) {
	env := map[string]string{}
	applyMemorySearchPolicyEnv(env, &registry.SwarmRole{MemorySearch: &registry.MemorySearchPolicy{
		RepoScope: "github.com/x/y", StrictScope: true, Authority: "provenance", Limit: 8}})
	want := map[string]string{"VORNIK_MEM_SCOPE": "github.com/x/y", "VORNIK_MEM_STRICT": "true", "VORNIK_MEM_AUTHORITY": "provenance", "VORNIK_MEM_LIMIT": "8"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}

	// No policy: no variable, and a variable the role's own envVars set is
	// removed, so a hand-written env cannot impersonate a pinned policy.
	env = map[string]string{"VORNIK_MEM_AUTHORITY": "any", "OTHER": "kept"}
	applyMemorySearchPolicyEnv(env, &registry.SwarmRole{})
	if _, ok := env["VORNIK_MEM_AUTHORITY"]; ok || env["OTHER"] != "kept" {
		t.Errorf("env %v; want the VORNIK_MEM_* keys cleared and others kept", env)
	}
}

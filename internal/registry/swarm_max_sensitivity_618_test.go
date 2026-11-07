package registry

import (
	"strings"
	"testing"
)

// #618 C1 (firewall design, "Amendment 2026-11-06 (#618)"): the loader rules
// for memorySearch.maxSensitivity. Before the change the field did not exist.
func TestSwarmValidate_MaxSensitivity_618(t *testing.T) {
	const scope = "github.com/EaseIT-cz/vornik"
	for _, tc := range []struct {
		name    string
		policy  *MemorySearchPolicy
		tools   []string
		wantErr string
	}{
		{"public", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "public"}, nil, ""},
		{"internal", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "internal"}, nil, ""},
		{"confidential", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "confidential"}, nil, ""},
		{"restricted is not a cap", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "restricted"}, nil, "maxSensitivity"},
		{"an unknown tier", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "secret"}, nil, "maxSensitivity"},
		{"a cap without scope", &MemorySearchPolicy{MaxSensitivity: "public"}, nil, "repoScope is required"},
		{"a cap on a role with run_shell", &MemorySearchPolicy{RepoScope: scope, MaxSensitivity: "public"}, []string{"run_shell"}, "run_shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := memorySearchSwarm(tc.policy, tc.tools...).Validate("s.md")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
	if !(&MemorySearchPolicy{MaxSensitivity: "public"}).Narrows() {
		t.Error("a cap must count as narrowing")
	}
}

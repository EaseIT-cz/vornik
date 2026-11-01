package registry

// GitHub #550 (found at: production, 2026-10-25). Design:
// https://docs.vornik.io §2.1 C1. A role
// may pin a retrieval policy for memory_search; the loader refuses one that
// cannot mean what it says.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func memorySearchSwarm(policy *MemorySearchPolicy, tools ...string) *Swarm {
	return &Swarm{ID: "s", Roles: []SwarmRole{{
		Name:         "consultant",
		Runtime:      SwarmRoleRuntime{Image: "img"},
		Permissions:  SwarmRolePermissions{AllowedTools: append([]string{"memory_search"}, tools...)},
		MemorySearch: policy,
	}}}
}

func TestSwarmValidate_MemorySearchPolicy(t *testing.T) {
	const scope = "github.com/EaseIT-cz/vornik"
	cases := []struct {
		name    string
		policy  *MemorySearchPolicy
		tools   []string
		wantErr string // "" = valid
	}{
		{"no policy", nil, nil, ""},
		{"full policy", &MemorySearchPolicy{RepoScope: scope, StrictScope: true, Authority: "provenance", Limit: 8}, nil, ""},
		{"lenient with provenance", &MemorySearchPolicy{RepoScope: scope, Authority: "provenance"}, nil, ""},
		{"scope only", &MemorySearchPolicy{RepoScope: scope}, nil, ""},
		{"authority without scope", &MemorySearchPolicy{Authority: "provenance"}, nil, "repoScope is required"},
		{"strict without scope", &MemorySearchPolicy{StrictScope: true}, nil, "repoScope is required"},
		{"unknown authority", &MemorySearchPolicy{RepoScope: scope, Authority: "role-of-record"}, nil, "authority"},
		{"limit above the tool cap", &MemorySearchPolicy{RepoScope: scope, Limit: 21}, nil, "limit"},
		{"negative limit", &MemorySearchPolicy{RepoScope: scope, Limit: -1}, nil, "limit"},
		{"narrowing policy on a role with run_shell", &MemorySearchPolicy{RepoScope: scope, Authority: "provenance"}, []string{"run_shell"}, "run_shell"},
		{"strict policy on a role with run_shell", &MemorySearchPolicy{RepoScope: scope, StrictScope: true}, []string{"run_shell"}, "run_shell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := memorySearchSwarm(tc.policy, tc.tools...).Validate("s.md")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error mentioning %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), "memorySearch"):
				t.Fatalf("error %q does not name the memorySearch field", err)
			}
		})
	}
}

func TestSwarmRole_MemorySearchPolicyParsesFromYAML(t *testing.T) {
	var role SwarmRole
	src := "name: consultant\nmemorySearch:\n  repoScope: github.com/x/y\n  strictScope: true\n  authority: provenance\n  limit: 8\n"
	if err := yaml.Unmarshal([]byte(src), &role); err != nil {
		t.Fatal(err)
	}
	got := role.MemorySearch
	if got == nil || got.RepoScope != "github.com/x/y" || !got.StrictScope || got.Authority != "provenance" || got.Limit != 8 {
		t.Fatalf("parsed %+v", got)
	}
}

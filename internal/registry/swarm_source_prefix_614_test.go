package registry

import (
	"strings"
	"testing"
)

// #614 C3: the loader rules for memorySearch.sourcePrefixes. Before the
// change the field did not exist.
func TestSwarmValidate_SourcePrefixes_614(t *testing.T) {
	const scope = "github.com/EaseIT-cz/vornik"
	ok := []string{"docs/architect/", "docs/public/"}
	for _, tc := range []struct {
		name    string
		policy  *MemorySearchPolicy
		tools   []string
		wantErr string
	}{
		{"the architect's policy", &MemorySearchPolicy{RepoScope: scope, StrictScope: true, Authority: "provenance", SourcePrefixes: ok}, nil, ""},
		{"prefixes without scope", &MemorySearchPolicy{SourcePrefixes: ok}, nil, "repoScope is required"},
		{"no trailing slash", &MemorySearchPolicy{RepoScope: scope, SourcePrefixes: []string{"docs/public"}}, nil, "sourcePrefixes"},
		{"an empty entry", &MemorySearchPolicy{RepoScope: scope, SourcePrefixes: []string{"docs/public/", ""}}, nil, "sourcePrefixes"},
		{"a wildcard", &MemorySearchPolicy{RepoScope: scope, SourcePrefixes: []string{"docs/%/"}}, nil, "sourcePrefixes"},
		{"a parent segment", &MemorySearchPolicy{RepoScope: scope, SourcePrefixes: []string{"docs/../legal/"}}, nil, "sourcePrefixes"},
		{"prefixes on a role with run_shell", &MemorySearchPolicy{RepoScope: scope, SourcePrefixes: ok}, []string{"run_shell"}, "run_shell"},
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
}

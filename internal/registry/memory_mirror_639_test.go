package registry

import (
	"strings"
	"testing"
)

// #639 (design 22, "Amendment 2026-11-04 (#639)", M1): memory.mirror_paths
// lists repository prefixes the provenance sweep mirrors into the project's
// memory. Each is a valid source prefix (trailing "/", no "." or ".."
// segment), and the setting needs a repo_scope.
func TestMemoryMirrorPathsValidation_639(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
		paths []string
		want  string
	}{
		{"unset", "", nil, ""},
		{"two prefixes", "github.com/acme/widgets", []string{"docs/architect/", "docs/public/"}, ""},
		{"no trailing slash", "github.com/acme/widgets", []string{"docs/public"}, "memory.mirror_paths"},
		{"dot-dot segment", "github.com/acme/widgets", []string{"docs/../secrets/"}, "memory.mirror_paths"},
		{"empty entry", "github.com/acme/widgets", []string{""}, "memory.mirror_paths"},
		{"no repo_scope", "", []string{"docs/public/"}, "repo_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Project{RepoScope: tc.scope}
			p.Memory.MirrorPaths = tc.paths
			err := p.validateMemoryMirror("p.yaml")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

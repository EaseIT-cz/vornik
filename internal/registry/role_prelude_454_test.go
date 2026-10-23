package registry

import (
	"strings"
	"testing"
)

// GitHub #454 (audit 2026-10-17, found at: review): the always-on role
// prelude said only that <untrusted_content> blocks are data. It never said
// memory_search results are untrusted, nor what an escaped marker inside a
// block means (#453). Every agent role's effective prompt must carry both.
func TestBuildEffectiveRolePrompt454_NamesUntrustedToolResultsAndEscapedMarker(t *testing.T) {
	got := BuildEffectiveRolePrompt(nil, SwarmRole{})
	for _, want := range []string{"memory_search", "&lt;/untrusted_content>", "whether or not they are marked"} {
		if !strings.Contains(got, want) {
			t.Errorf("effective role prompt lacks %q:\n%s", want, got)
		}
	}
}

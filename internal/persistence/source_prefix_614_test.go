package persistence

import "testing"

// #614 C3: the one rule for a retrieval source prefix, shared by the swarm
// loader and the memory search SQL.
func TestValidSourcePrefix_614(t *testing.T) {
	for _, ok := range []string{"docs/public/", "docs/architect/", "a/", "docs/architect_x/", "v1.2-x/"} {
		if !ValidSourcePrefix(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "/", "docs/public", "/docs/public/", "docs//public/", "docs/../legal/", "./docs/",
		"docs/%/", "docs/x'/", `docs\x/`, "docs/ x/", "docs/é/"} {
		if ValidSourcePrefix(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}

package imagemanifest

import (
	"sync"
	"testing"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// the agent image moved from ghcr.io/grinco to ghcr.io/easeit-cz with its
// history copied digest-for-digest. Deployed configs are never overwritten,
// so they keep naming the old repository; CanonicalImageRef maps that name,
// and only that name, to the new one, keeping the tag or digest.
func TestCanonicalImageRef(t *testing.T) {
	const d = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ in, want string }{
		{"ghcr.io/grinco/vornik-agent:latest", "ghcr.io/easeit-cz/vornik-agent:latest"},
		{"ghcr.io/grinco/vornik-agent:2026.9.8", "ghcr.io/easeit-cz/vornik-agent:2026.9.8"},
		{"ghcr.io/grinco/vornik-agent:sha-123456789012", "ghcr.io/easeit-cz/vornik-agent:sha-123456789012"},
		{"ghcr.io/grinco/vornik-agent@" + d, "ghcr.io/easeit-cz/vornik-agent@" + d},
		{"ghcr.io/grinco/vornik-agent", "ghcr.io/easeit-cz/vornik-agent"},
		// Left alone: another image of the old owner, a longer name that
		// shares the prefix, local and foreign images, empty, already new.
		{"ghcr.io/grinco/other:latest", "ghcr.io/grinco/other:latest"},
		{"ghcr.io/grinco/vornik-agent-x:latest", "ghcr.io/grinco/vornik-agent-x:latest"},
		{"localhost/vornik-agent:latest", "localhost/vornik-agent:latest"},
		{"docker.io/library/postgres:16", "docker.io/library/postgres:16"},
		{"", ""},
		{"ghcr.io/easeit-cz/vornik-agent:latest", "ghcr.io/easeit-cz/vornik-agent:latest"},
	} {
		got := CanonicalImageRef(tc.in)
		if got != tc.want {
			t.Errorf("CanonicalImageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if again := CanonicalImageRef(got); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}

func TestAgentImageTagIsTheNewRegistry(t *testing.T) {
	if AgentImageTag != "ghcr.io/easeit-cz/vornik-agent:latest" {
		t.Fatalf("AgentImageTag = %q", AgentImageTag)
	}
	if CanonicalImageRef(AgentImageTag) != AgentImageTag {
		t.Fatal("the canonical name must map to itself")
	}
}

// A rewrite is visible, once per reference per process, not silent.
func TestLegacyImageNoteIsLoggedOnce(t *testing.T) {
	var mu sync.Mutex
	var got []string
	restore := SetLegacyImageLogger(func(old, canonical, source string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, old+"|"+canonical+"|"+source)
	})
	defer restore()
	resetLegacyImageNotes()

	for i := 0; i < 3; i++ {
		CanonicalImageRefFrom("ghcr.io/grinco/vornik-agent:latest", "swarms/a.md")
	}
	CanonicalImageRefFrom("ghcr.io/easeit-cz/vornik-agent:latest", "swarms/b.md")
	CanonicalImageRefFrom("localhost/x:1", "swarms/c.md")
	if len(got) != 1 || got[0] != "ghcr.io/grinco/vornik-agent:latest|ghcr.io/easeit-cz/vornik-agent:latest|swarms/a.md" {
		t.Fatalf("notes = %q, want exactly one for the legacy reference", got)
	}
}

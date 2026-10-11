package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #119 item 6 (2026-10-08): a user's "forget this" is recorded as the
// user's own forget from any harness, not only from Hermes; the mirror's
// route stays with clients whose memory file mirrors into Vornik (LLD 22,
// "Memory the user can see", change §2).

func TestRefuteRoute_ForgetCommandFromAnyClient(t *testing.T) {
	ids := []string{"c1"}
	for _, kind := range []string{"claude-code", "codex", "goose", "hermes"} {
		key := &persistence.APIKey{ClientKind: kind}
		assert.Equal(t, memory.RefuteRouteForgetCommand, companionRefuteRoute(key, "forget_command", ids), kind)
		assert.Equal(t, memory.RefuteRouteMemoryCorrect, companionRefuteRoute(key, "forget_command", nil), "%s claim mode", kind)
	}
}

func TestRefuteRoute_MirrorForgetStillHermesOnly(t *testing.T) {
	ids := []string{"c1"}
	for _, kind := range []string{"claude-code", "codex", "goose", ""} {
		assert.Equal(t, memory.RefuteRouteMemoryCorrect, companionRefuteRoute(&persistence.APIKey{ClientKind: kind}, "mirror_forget", ids), kind)
	}
	assert.Equal(t, memory.RefuteRouteMirrorForget, companionRefuteRoute(&persistence.APIKey{ClientKind: "hermes"}, "mirror_forget", ids))
	// A device route is never accepted from a key.
	assert.Equal(t, memory.RefuteRouteMemoryCorrect, companionRefuteRoute(&persistence.APIKey{ClientKind: "hermes"}, "device_forget", ids))
}

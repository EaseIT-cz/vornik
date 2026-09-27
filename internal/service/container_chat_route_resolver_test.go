package service

import (
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

// Issue #61(b), at the level the doctor now reads it (model-route-coverage
// design, 2026-09-24): gpt-5.4 is served by codex-subscription through a
// DEFAULT route merged in because that sub-provider is enabled. The operator's
// routes never name "gpt-". The router the container built must say it is
// routed — the doctor used to answer from the operator's list and said not.
func TestInitChatRouter_KeepsTheRouterItBuiltAndItRoutesMergedDefaults(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Router: config.ChatRouterConfig{
			Default: "codex-subscription",
			CodexSubscription: config.ChatCodexSubscriptionSubConfig{
				Enabled:  true,
				AuthPath: filepath.Join(t.TempDir(), "auth.json"),
			},
			Routes: []config.ChatRouteConfig{{Prefix: "zai.", Kind: "codex-subscription"}},
		},
	}
	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.chatRouter, "the container must keep the router so the doctor can ask it")

	route, matched := c.chatRouter.Resolves("gpt-5.4")
	assert.True(t, matched, "gpt-5.4 reads as unrouted — the issue #61(b) finding")
	assert.Equal(t, "codex-subscription", route)
}

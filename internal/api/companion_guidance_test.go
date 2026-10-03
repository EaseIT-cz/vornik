package api

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Companion RAG-first guidance design §10 (2026-10-03): the operator's rules
// for working alongside Vornik travel with the daemon and the plugins, for
// every agent, after a session left its Vornik team idle (2026-10-02).

// The embedded text is the file: no copy drifts at build time (review e485 F10).
func TestCompanionGuidance_EmbedRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("companion_guidance.md")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(raw)), CompanionGuidance())
	require.Contains(t, CompanionGuidance(), "(delegation)", "the self-gate names the delegation rules (review a950 F1)")
}

// The claude-code and codex delegate skills carry the text verbatim between
// markers; the SessionStart hook prints that section (review e485 F5).
func TestCompanionGuidance_DelegateSkillsCarryItVerbatim(t *testing.T) {
	for _, p := range []string{
		"../../contrib/claude-code-companion/skills/delegate/SKILL.md",
		"../../contrib/codex-companion/skills/delegate/SKILL.md",
	} {
		raw, err := os.ReadFile(p)
		require.NoError(t, err)
		_, rest, ok := strings.Cut(string(raw), companionGuidanceStart)
		require.True(t, ok, "%s has no %s marker", p, companionGuidanceStart)
		body, _, ok := strings.Cut(rest, companionGuidanceEnd)
		require.True(t, ok, "%s has no %s marker", p, companionGuidanceEnd)
		require.Equal(t, CompanionGuidance(), strings.TrimSpace(body), "%s: copy internal/api/companion_guidance.md between the markers", p)
	}
}

// Who receives it is decided by the daemon (review e485 F2, F4; 1e7f F1):
// a delegating key and a memory-only key get it (the self-gate drops the
// delegation rules for the latter); a broker-project key gets none.
func TestCompanionGuidance_InitializeByKeyKind(t *testing.T) {
	srv, keyRepo, _ := newBrokerMCPServer(t)
	instructions := func(raw string) string {
		rec := httptest.NewRecorder()
		srv.CompanionMCPHandler(rec, withCompanionBearer(mcpRequest(t, "initialize", nil), raw))
		resp := decodeJSONRPC(t, rec.Body.Bytes())
		require.Nil(t, resp.Error)
		m, _ := resp.Result.(map[string]any)
		s, _ := m["instructions"].(string)
		return s
	}
	memRaw, _ := seedCompanionKey(t, keyRepo, "memory-acme", nil)
	keyRepo.rows[len(keyRepo.rows)-1].DelegateDisabled = true
	plainRaw, _ := seedCompanionKey(t, keyRepo, "memory-acme", nil)
	brokerRaw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)

	require.Equal(t, CompanionGuidance(), instructions(plainRaw), "a delegating companion key")
	require.Equal(t, CompanionGuidance(), instructions(memRaw), "a memory-only key")
	require.Empty(t, instructions(brokerRaw), "a broker-project key")
}

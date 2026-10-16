package api

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Companion RAG-first guidance design §11 (2026-10-15, GitHub #382): the
// development guidance (RAG first, design first, keep memory current, the
// as-built bootstrap) ships with every bundle, for any project, from one
// source.

// The embedded text is the file.
func TestCompanionDevGuidance_EmbedRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("companion_dev_guidance.md")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(raw)), CompanionDevGuidance())
	require.Contains(t, CompanionDevGuidance(), "design_record",
		"the text adapts to what memory holds through the recall's design_record (§11.2)")
}

// Each bundle's develop-with-memory skill carries the text verbatim between
// the markers; the Claude Code SessionStart hook prints that section.
func TestCompanionDevGuidance_SkillsCarryItVerbatim(t *testing.T) {
	for _, p := range []string{
		"../../contrib/claude-code-companion/skills/develop-with-memory/SKILL.md",
		"../../contrib/codex-companion/skills/develop-with-memory/SKILL.md",
		"../../contrib/hermes-companion/skills/develop-with-memory/SKILL.md",
	} {
		raw, err := os.ReadFile(p)
		require.NoError(t, err)
		_, rest, ok := strings.Cut(string(raw), companionDevGuidanceStart)
		require.True(t, ok, "%s has no %s marker", p, companionDevGuidanceStart)
		body, _, ok := strings.Cut(rest, companionDevGuidanceEnd)
		require.True(t, ok, "%s has no %s marker", p, companionDevGuidanceEnd)
		require.Equal(t, CompanionDevGuidance(), strings.TrimSpace(body),
			"%s: copy internal/api/companion_dev_guidance.md between the markers", p)
	}
}

// Part B (design first) is delimited exactly once, start before end, so the
// hook's VORNIK_DESIGN_FIRST=off can drop it and nothing else (§11.3).
func TestCompanionDevGuidance_DesignFirstMarkers(t *testing.T) {
	g := CompanionDevGuidance()
	require.Equal(t, 1, strings.Count(g, companionDesignFirstStart))
	require.Equal(t, 1, strings.Count(g, companionDesignFirstEnd))
	start := strings.Index(g, companionDesignFirstStart)
	end := strings.Index(g, companionDesignFirstEnd)
	require.Less(t, start, end)
	partA, partB := g[:start], g[start:end]
	require.Contains(t, partA, "Recall before reading code", "part A keeps the investigation rules")
	require.Contains(t, partA, "Keep memory current", "part A keeps the keep-current rule")
	require.Contains(t, partB, "as-built", "part B holds the bootstrap")
	require.NotContains(t, partA, "as-built", "the bootstrap lives in part B only")
}

// B1 of review 97b7: nothing in the text may assert that a design exists.
func TestCompanionDevGuidance_NeverClaimsADesignExists(t *testing.T) {
	g := CompanionDevGuidance()
	require.NotContains(t, g, "are the authoritative design record")
	require.Contains(t, g, "do not invent one")
}

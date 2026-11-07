package executor

// GitHub #633 (reviewable-output design §6.1): every agent is told how to
// write for a person, through an ADVISORY block compiled into the binary, so
// an upgrade reaches every install. An operator may switch it off per swarm
// or per role.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/promptblock"
)

// writingStyleMaxBytes is the block's own ceiling (design §6.1): the shipped
// text is trimmed to leave the next block room under the 3,000-byte aggregate.
const writingStyleMaxBytes = 560

func TestWritingStyle_BlockWithinItsCeiling(t *testing.T) {
	require.LessOrEqual(t, len(writingStyleSystemPromptBlock), writingStyleMaxBytes)
	require.Contains(t, writingStyleSystemPromptBlock, "path:line")
}

func TestWritingStyle_IsAdvisory(t *testing.T) {
	c, ok := promptblock.ClassOf(promptblock.WritingStyle)
	require.True(t, ok)
	require.Equal(t, promptblock.Advisory, c)
}

func TestWritingStyle_InEveryComposedPrompt(t *testing.T) {
	sp := systemPromptFor(t, baseOpts())
	require.Contains(t, sp, strings.TrimSpace(writingStyleSystemPromptBlock))
	// Appended after the role identity, never before it (LLD 09 §13.3(3)).
	require.Less(t, strings.Index(sp, "You are the worker role."), strings.Index(sp, strings.TrimSpace(writingStyleSystemPromptBlock)))
}

func TestWritingStyle_SuppressedByTheList(t *testing.T) {
	opts := baseOpts()
	opts.SuppressedGuidanceBlocks = []string{promptblock.WritingStyle}
	sp := systemPromptFor(t, opts)
	require.NotContains(t, sp, strings.TrimSpace(writingStyleSystemPromptBlock))
	require.Contains(t, sp, strings.TrimSpace(claimVerificationSystemPromptBlock), "the invariant block stays")
}

// A tool-free route step returns a schema-bound pick, not prose for a person.
func TestWritingStyle_NotOnAToolFreeStep(t *testing.T) {
	opts := baseOpts()
	opts.ToolFree = true
	sp := systemPromptFor(t, opts)
	require.NotContains(t, sp, strings.TrimSpace(writingStyleSystemPromptBlock))
}

func TestWritingStyle_AttributedInPromptCost(t *testing.T) {
	cost := AttributePromptCost(PromptCostFixture{Role: "worker", RolePrompt: "You are the worker role."})
	found := false
	for _, s := range cost.Sources {
		if s.Name == promptblock.WritingStyle {
			found = true
			require.Greater(t, s.Bytes, 0)
		}
	}
	require.True(t, found, "the cost attribution mirrors the composition: %+v", cost.Sources)
	cost = AttributePromptCost(PromptCostFixture{Role: "worker", RolePrompt: "You are the worker role.",
		SuppressedGuidanceBlocks: []string{promptblock.WritingStyle}})
	for _, s := range cost.Sources {
		require.NotEqual(t, promptblock.WritingStyle, s.Name)
	}
}

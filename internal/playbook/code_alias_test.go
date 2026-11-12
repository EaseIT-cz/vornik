package playbook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/stepoutcome"
)

// GitHub #702 (found in production 2026-11-09): `vornikctl doctor` model_health
// tells the operator that most failures were "the provider refusing the
// account for a usage or rate limit (UPSTREAM_RATE_LIMITED)". The obvious next
// step, `vornikctl playbook show UPSTREAM_RATE_LIMITED`, answered "Unrecognised
// failure class" because UPSTREAM_RATE_LIMITED is an error_detail CODE
// (chat.CodeUpstreamRateLimited), not a stepoutcome class — and the remediation
// sits inside the LLM_CALL_FAILED entry. Lookup must resolve the code to the
// entry that covers it, and say so.

// TestLookup_ResolvesErrorDetailCodeToCoveringEntry — the regression for #702:
// a code doctor prints resolves to the corpus entry that covers it, not the
// unknown-class fallback. Fails pre-fix (Lookup returned the fallback).
func TestLookup_ResolvesErrorDetailCodeToCoveringEntry(t *testing.T) {
	got := Lookup(chat.CodeUpstreamRateLimited)
	assert.Equal(t, stepoutcome.ClassLLMCallFailed, got.Class,
		"the code must resolve to the entry that covers it")
	assert.Equal(t, chat.CodeUpstreamRateLimited, got.ResolvedFrom,
		"the resolved entry must name the code it was looked up under, so the surface can say so")
	assert.NotContains(t, got.Cause, "Unrecognised",
		"resolving a known code must not hit the unknown-class fallback")
}

// TestLookup_AliasNeverPointsAtAMissingEntry — an alias whose target is not in
// the corpus would silently degrade back to the fallback, which is the defect
// this feature exists to remove. Every alias must name a real entry.
func TestLookup_AliasNeverPointsAtAMissingEntry(t *testing.T) {
	for code, class := range codeAliases {
		t.Run(code, func(t *testing.T) {
			_, ok := corpus[class]
			require.True(t, ok, "alias %q points at %q, which has no corpus entry", code, class)
		})
	}
}

// TestLookup_ClassBeatsAlias — guards the resolution ORDER: Lookup checks the
// corpus before codeAliases, so an identifier that is BOTH a class and an
// alias key answers as the class (ResolvedFrom empty); the alias only fires on
// a corpus miss. No alias key collides with a class today, so pin that
// disjointness — the moment one does, this fails and forces the collision to
// be handled deliberately instead of resolving silently one way.
func TestLookup_ClassBeatsAlias(t *testing.T) {
	for code := range codeAliases {
		_, isClass := corpus[code]
		assert.False(t, isClass,
			"%s is both an alias key and a corpus class; Lookup would answer as the class — "+
				"revisit the corpus-before-aliases order and this guard", code)
	}
}

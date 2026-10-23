//go:build integration
// +build integration

package integration_test

// GitHub #482 round-3 review B5 (design 22, "Amendment 2026-10-21 (#482)",
// part 8f). A claim refute matches text: a companion's memory_correct claim
// that matched a provenance-verified design chunk (the merged bytes of the
// design) refuted it, retiring the verified design record. Found at: review;
// a user reaches it at the companion memory_correct tool, which is where
// this test sits. Now the claim path skips a provenance-verified chunk
// (skip reason provenance_verified); an explicit chunk_ids forget still
// retires it.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntegration_CompanionMemoryCorrectSparesProvenanceVerified(t *testing.T) {
	h := newGH368Harness(t)
	design := h.chunk(t, "design", "", "The quorlin cache keeps entries for ninety days on every node.")
	_, err := h.db.Exec(`UPDATE project_memory_chunks SET validation_status = 'verified',
		validator_role = 'provenance:origin/main', producer_role = 'rag-ingester' WHERE id = $1`, design)
	require.NoError(t, err)

	out := h.call(t, "memory_correct", map[string]any{"wrong_claim": "quorlin cache keeps entries for ninety days"})
	require.NotContains(t, gh436IDs(out, "refuted"), design, "a claim refuted the provenance-verified design: %v", out)
	require.Equal(t, "verified", gh436Status(t, h, design))
	require.Equal(t, "provenance_verified", gh436SkipReason(out, design), "the design must be reported as skipped: %v", out)

	// Design review 565b finding 1: naming the chunk by id with no forget
	// reason (route memory_correct) is the same companion "this is wrong"
	// as the claim, and spares it too.
	byID := h.call(t, "memory_correct", map[string]any{"chunk_ids": []string{design}, "reason": "wrong"})
	require.Equal(t, "verified", gh436Status(t, h, design), "a by-id memory_correct retired the provenance-verified design: %v", byID)

	// The explicit forget is the user's to keep.
	forget := h.call(t, "memory_correct", map[string]any{"chunk_ids": []string{design}, "reason": "forget_command"})
	require.Equal(t, "refuted", gh436Status(t, h, design), "%v", forget)
}

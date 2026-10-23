//go:build integration
// +build integration

package integration_test

// GitHub #436 (design 22, "Amendment 2026-10-21 (#436): a claim refute never
// refutes an operator correction"). Found at: review (the 2026-10-17 security
// audit); a user observes it at the companion memory_correct tool. Regression
// test at: the companion MCP tools/call handler, through the production
// adapter, Corrector, Searcher and repository on the real pgvector database.
//
// The defect: memory_correct's claim search did not exclude the operator's
// own corrections, so a later claim about the same subject refuted the
// standing correction instead of the stale chunk.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func gh436Status(t *testing.T, h *gh368Harness, id string) string {
	t.Helper()
	var status string
	require.NoError(t, h.db.QueryRow(`SELECT validation_status FROM project_memory_chunks WHERE id = $1`, id).Scan(&status))
	return status
}

func gh436IDs(out map[string]any, field string) []string {
	items, _ := out[field].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["chunk_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func gh436SkipReason(out map[string]any, id string) string {
	items, _ := out["skipped"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["chunk_id"] == id {
			r, _ := m["reason"].(string)
			return r
		}
	}
	return ""
}

// The operator's correction from an earlier call is not refuted by a later
// claim that matches it, and the caller is told it was passed over.
func TestIntegration_CompanionMemoryCorrectSparesOperatorCorrection(t *testing.T) {
	h := newGH368Harness(t)
	stale := h.chunk(t, "stale", "", "The vexmoor relay listens on port 8080 on every node.")

	first := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "vexmoor relay listens on port 8080",
		"correction":  "The vexmoor relay listens on port 9090 on every node.",
	})
	require.Equal(t, []string{stale}, gh436IDs(first, "refuted"), "the first correction must refute the stale chunk: %v", first)
	correction, _ := first["correction_chunk_id"].(string)
	require.NotEmpty(t, correction, "%v", first)
	require.NotEqual(t, "refuted", gh436Status(t, h, correction))

	second := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "vexmoor relay listens on port 9090",
		"max_refutes": 1,
	})
	require.NotContains(t, gh436IDs(second, "refuted"), correction, "a claim refuted the operator's own correction: %v", second)
	require.NotEqual(t, "refuted", gh436Status(t, h, correction), "the correction must not be refuted")
	require.Equal(t, "protected_correction", gh436SkipReason(second, correction), "the protected correction must be reported as skipped: %v", second)
}

// With max_refutes=1 and both a correction and an unverified chunk matching
// the claim, the unverified chunk is the one refuted.
func TestIntegration_CompanionMemoryCorrectPrefersStaleOverCorrection(t *testing.T) {
	h := newGH368Harness(t)
	first := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "brimlock gateway uses socket 4100",
		"correction":  "The brimlock gateway uses socket 4200 behind the cache tier.",
	})
	correction, _ := first["correction_chunk_id"].(string)
	require.NotEmpty(t, correction, "%v", first)
	stale := h.chunk(t, "stale", "", "Old runbook: the brimlock gateway uses socket 4200.")

	out := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "brimlock gateway uses socket 4200 behind the cache tier",
		"max_refutes": 1,
	})
	require.NotEqual(t, "refuted", gh436Status(t, h, correction), "the correction was refuted: %v", out)
	require.Equal(t, []string{stale}, gh436IDs(out, "refuted"), "%v", out)
	require.Equal(t, "protected_correction", gh436SkipReason(out, correction), "%v", out)
	require.Equal(t, "refuted", gh436Status(t, h, stale))
}

// dry_run reports what a claim would refute and writes nothing: no refute,
// no correction.
func TestIntegration_CompanionMemoryCorrectDryRunWritesNothing(t *testing.T) {
	h := newGH368Harness(t)
	stale := h.chunk(t, "stale", "", "The quornfield cache evicts after 600 seconds on every node.")

	out := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "quornfield cache evicts after 600 seconds",
		"correction":  "The quornfield cache evicts after 900 seconds.",
		"dry_run":     true,
	})
	require.Equal(t, true, out["dry_run"], "%v", out)
	require.Empty(t, gh436IDs(out, "refuted"), "%v", out)
	require.Equal(t, []string{stale}, gh436IDs(out, "would_refute"), "%v", out)
	require.Empty(t, out["correction_chunk_id"], "a dry run stored a correction: %v", out)
	require.NotEqual(t, "refuted", gh436Status(t, h, stale), "a dry run refuted a chunk")

	var n int
	require.NoError(t, h.db.QueryRow(`SELECT count(*) FROM project_memory_chunks WHERE project_id = $1`, h.project).Scan(&n))
	require.Equal(t, 1, n, "the project must hold only the seeded chunk after a dry run; another row means the dry run stored one")
	require.NoError(t, h.db.QueryRow(`SELECT count(*) FROM project_memory_chunks WHERE project_id = $1 AND (producer_role = 'operator_correction' OR artifact_id LIKE 'companioncorrection\_%')`, h.project).Scan(&n))
	require.Zero(t, n, "a dry run stored a correction")
}

// A companion correction is a companion deposit (GitHub #436, W16-E resume;
// design 22 "Amendment 2026-10-21 (#436), part 2"): it passes the same gates
// as remember, so a secret in it is redacted, and it is stored under the
// companion's producer role, never as an operator correction. A later claim
// still never refutes it. Found at: review (lane W16-D, wave 16):
// InsertCorrection stored it verified, decision, 0.95, never expiring, with
// no secret scan.
func TestIntegration_CompanionMemoryCorrectionPassesTheCompanionGates(t *testing.T) {
	h := newGH368Harness(t)
	const secret = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	h.chunk(t, "stale", "", "The tarnwick deploy token is kept in the vault entry tarnwick/deploy.")
	first := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "tarnwick deploy token is kept in the vault entry",
		"correction":  "The tarnwick deploy token moved to the secrets store; the old value was " + secret + ".",
	})
	correction, _ := first["correction_chunk_id"].(string)
	require.NotEmpty(t, correction, "%v", first)

	var content, role string
	require.NoError(t, h.db.QueryRow(`SELECT content, COALESCE(producer_role, '') FROM project_memory_chunks WHERE id = $1`, correction).Scan(&content, &role))
	require.NotContains(t, content, secret, "a companion correction skipped the secret scan")
	require.Contains(t, content, "[REDACTED:")
	require.Equal(t, "companion:claude-code", role, "a companion correction must carry the companion producer role")

	second := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "tarnwick deploy token moved to the secrets store",
		"max_refutes": 1,
	})
	require.NotContains(t, gh436IDs(second, "refuted"), correction, "a claim refuted a companion correction: %v", second)
	require.Equal(t, "protected_correction", gh436SkipReason(second, correction), "%v", second)
}

// The correction is stored before the refute, and a correction the gates do
// not admit holds the refute (review ee12 F1): the call never demotes the
// old fact and loses the new one. A byte-identical earlier deposit makes the
// dedup gate refuse the correction.
func TestIntegration_CompanionMemoryCorrectionNotStoredHoldsTheRefute(t *testing.T) {
	h := newGH368Harness(t)
	stale := h.chunk(t, "stale", "", "The glimmerfen queue drains every 30 seconds on every node.")
	const fix = "The glimmerfen queue drains every 45 seconds on every node."
	h.call(t, "remember", map[string]any{"content": fix})

	out := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "glimmerfen queue drains every 30 seconds",
		"correction":  fix,
	})
	require.NotEqual(t, "ALLOW", out["correction_decision"], "the duplicate correction was admitted; the test does not exercise the held refute: %v", out)
	require.Empty(t, gh436IDs(out, "refuted"), "a refute landed although the correction was not stored: %v", out)
	require.NotEqual(t, "refuted", gh436Status(t, h, stale))
}

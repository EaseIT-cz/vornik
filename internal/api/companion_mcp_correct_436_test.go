package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #436 (design 22, "Amendment 2026-10-21 (#436)"). Found at: review
// (the 2026-10-17 security audit). The behavioural regression test is on the
// pgvector lane (test/integration/companion_memory_correct_436_test.go);
// these pin the tools/call handler's dry_run argument and its report shape.

func callMemoryCorrect(t *testing.T, srv *Server, raw string, args map[string]any) (string, bool) {
	t.Helper()
	req := mcpRequest(t, "tools/call", map[string]any{"name": "memory_correct", "arguments": args})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	return decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
}

func TestCompanionMCP_MemoryCorrect_DryRunReachesAdapterAndReportsWouldRefute(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{correctReturn: CorrectResult{
		DryRun:  true,
		Refuted: []RefutedChunkInfo{{ChunkID: "stale", Score: 0.4}},
		Skipped: []SkippedChunkInfo{{ChunkID: "corr", Score: 0.9, ValidationStatus: "verified", Reason: "protected_correction"}},
	}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	text, isErr := callMemoryCorrect(t, srv, raw, map[string]any{
		"wrong_claim": "X listens on 9090",
		"correction":  "X listens on 7070",
		"dry_run":     true,
	})
	require.False(t, isErr, text)
	require.Len(t, fake.correctCalls, 1)
	assert.True(t, fake.correctCalls[0].DryRun, "dry_run must reach the adapter")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["dry_run"])
	assert.Empty(t, out["refuted"], "a dry run reports would_refute, not refuted")
	assert.Equal(t, float64(0), out["refuted_count"])
	would, _ := out["would_refute"].([]any)
	require.Len(t, would, 1)
	skipped, _ := out["skipped"].([]any)
	require.Len(t, skipped, 1)
	assert.Equal(t, "protected_correction", skipped[0].(map[string]any)["reason"])
}

func TestCompanionMCP_MemoryCorrect_DryRunRefusedWithChunkIDs(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	text, isErr := callMemoryCorrect(t, srv, raw, map[string]any{
		"chunk_ids": []string{"c1"},
		"dry_run":   true,
	})
	require.True(t, isErr, text)
	assert.Contains(t, text, "dry_run")
	assert.Empty(t, fake.correctCalls, "a refused dry run must not reach the adapter")
}

func TestCompanionMCP_MemoryCorrect_ReportsSkipped(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{correctReturn: CorrectResult{
		Skipped: []SkippedChunkInfo{{ChunkID: "corr", Score: 0.9, Reason: "protected_correction"}},
	}}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	text, isErr := callMemoryCorrect(t, srv, raw, map[string]any{"wrong_claim": "X listens on 9090"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	skipped, _ := out["skipped"].([]any)
	require.Len(t, skipped, 1)
	assert.Contains(t, out["note"], "chunk_ids", "with nothing refuted and corrections skipped, the note must point at chunk_ids")
}

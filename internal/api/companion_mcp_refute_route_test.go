package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #76 (2026-10-05 audit), design 22 "Reinstating a refuted mirrored
// note" (round 2): memory_correct records the refuter's asserted route. The
// Hermes plugin's reason is honoured only from a hermes key in chunk-id mode;
// every other combination records memory_correct.
func TestCompanionMCP_MemoryCorrect_RefuteRoute(t *testing.T) {
	cases := []struct {
		name       string
		clientKind string
		args       map[string]any
		want       string
	}{
		{"hermes mirror forget", "hermes", map[string]any{"chunk_ids": []any{"c1"}, "reason": "mirror_forget"}, "mirror_forget"},
		{"hermes forget command", "hermes", map[string]any{"chunk_ids": []any{"c1"}, "reason": "forget_command"}, "forget_command"},
		{"hermes no reason", "hermes", map[string]any{"chunk_ids": []any{"c1"}}, "memory_correct"},
		{"hermes unknown reason", "hermes", map[string]any{"chunk_ids": []any{"c1"}, "reason": "because"}, "memory_correct"},
		{"hermes claim mode", "hermes", map[string]any{"wrong_claim": "dentist is Novak", "reason": "mirror_forget"}, "memory_correct"},
		{"claude-code declares mirror", "claude-code", map[string]any{"chunk_ids": []any{"c1"}, "reason": "mirror_forget"}, "memory_correct"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, keyRepo, _ := newCompanionMCPServer(t)
			fake := &fakeMemoryCompanion{correctReturn: CorrectResult{ByID: true, RefutedCount: 1}}
			srv.memoryCompanion = fake
			raw, err := apikey.Generate("alpha")
			require.NoError(t, err)
			require.NoError(t, keyRepo.Create(context.Background(), &persistence.APIKey{
				ID: "akey-route", ProjectID: "alpha", Name: "mem", KeyHash: apikey.Hash(raw),
				KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: tc.clientKind, SessionLabel: "t",
				MemoryRead: true, MemoryWrite: true, CreatedAt: time.Now().UTC(),
			}))
			req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{
				"name": "memory_correct", "arguments": tc.args,
			}), raw)
			rec := httptest.NewRecorder()
			srv.CompanionMCPHandler(rec, req)
			text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
			require.False(t, isErr, text)
			require.Len(t, fake.correctCalls, 1)
			require.Equal(t, tc.want, fake.correctCalls[0].RefuteRoute)
		})
	}
}

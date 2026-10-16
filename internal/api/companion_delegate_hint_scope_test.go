package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// GitHub #368, design 22 "Every search path honours repo_scope and
// strict_scope", entry point E3. Found at: review (the #142 census, wave 8
// lane N, 2026-10-14; re-read 2026-10-16). The delegate's recall hint searched
// the whole project: neither the delegate's repo_scope nor the key's default
// reached the Recall, so a delegation from repository A was hinted with
// repository B's notes. Regression test at: the MCP tools/call handler, the
// level a client observes the hint.
func delegateHintRecallScope(t *testing.T, keyScope string, args map[string]any) RecallOptions {
	t.Helper()
	srv, keyRepo, _ := newCompanionMCPServer(t)
	fake := &fakeMemoryCompanion{recallReturn: []MemorySearchResult{
		{ChunkID: "ck1", ProjectID: "alpha", SourceName: "decision/x", Content: "c", Score: 0.91},
	}}
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", []string{"wf-alpha"}, true, false)
	keyRepo.rows[len(keyRepo.rows)-1].DefaultRepoScope = keyScope
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "delegate", "arguments": args}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, text)
	require.Len(t, fake.recallCalls, 1, "the hint's recall did not run")
	return fake.recallCalls[0].Opts
}

func TestCompanionDelegate_RecallHintUsesDelegateScope(t *testing.T) {
	opts := delegateHintRecallScope(t, "", map[string]any{
		"workflow":   "wf-alpha",
		"prompt":     "Review the auth middleware refactor for compliance impact",
		"repo_scope": "github.com/example/repo-a",
	})
	require.Equal(t, "github.com/example/repo-a", opts.RepoScope)
	require.False(t, opts.StrictScope, "the hint uses the recall default: lenient")
}

func TestCompanionDelegate_RecallHintUsesKeyDefaultScope(t *testing.T) {
	opts := delegateHintRecallScope(t, "github.com/example/repo-a", map[string]any{
		"workflow": "wf-alpha",
		"prompt":   "Review the auth middleware refactor for compliance impact",
	})
	require.Equal(t, "github.com/example/repo-a", opts.RepoScope)
}

func TestCompanionDelegate_RecallHintArgOverridesKeyDefault(t *testing.T) {
	opts := delegateHintRecallScope(t, "github.com/example/repo-b", map[string]any{
		"workflow":   "wf-alpha",
		"prompt":     "Review the auth middleware refactor for compliance impact",
		"repo_scope": "github.com/example/repo-a",
	})
	require.Equal(t, "github.com/example/repo-a", opts.RepoScope)
}

func TestCompanionDelegate_RecallHintUnscopedWithoutAnyScope(t *testing.T) {
	opts := delegateHintRecallScope(t, "", map[string]any{
		"workflow": "wf-alpha",
		"prompt":   "Review the auth middleware refactor for compliance impact",
	})
	require.Equal(t, "", opts.RepoScope)
}

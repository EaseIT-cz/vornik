package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Legacy argument tolerance (LLD 21 §A, "Strict arguments: the clients the
// census missed", 2026-10-14; GitHub #305 follow-up).
//
// Found at: production-like (the 2026.10.6 release memory benchmark). A
// 2026.10.5 `vornikctl bench memory` sends recall "max_tokens" and a 2026.10.5
// `vornikctl bench agent` sends delegate "project" on every call; neither was
// ever declared, both were silently dropped before #305, and a 2026.10.6
// daemon refused them, so a shipped client broke on upgrade. These tests
// drive those exact shipped call shapes through the real handler.

func legacyCount(tool, arg, outcome string) float64 {
	return testutil.ToFloat64(companionLegacyArgCounter().WithLabelValues(tool, arg, outcome))
}

// The 2026.10.5 memory benchmark's recall: max_tokens beside the converted
// limit. Accepted, ignored, counted; the tool sees only declared arguments.
func TestCompanionLegacyArgs_RecallMaxTokensIgnored(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	mem := &fakeMemoryCompanion{recallReturn: []MemorySearchResult{{ChunkID: "c1", ProjectID: "alpha", SourceName: "s", Content: "hit", Score: 0.9}}}
	srv.memoryCompanion = mem
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	before := legacyCount("recall", "max_tokens", "ignored")
	text, isErr := callTool(t, srv, raw, "recall", map[string]any{
		"query": "q", "repo_scope": "lme/q1", "strict_scope": true, "sufficient": true,
		"limit": 8, "max_tokens": 4096,
	})
	require.False(t, isErr, "a 2026.10.5 bench memory recall was refused: %s", text)
	require.Len(t, mem.recallCalls, 1)
	assert.Equal(t, 8, mem.recallCalls[0].Opts.Limit, "the limit, not the ignored budget, bounds the result")
	assert.Equal(t, before+1, legacyCount("recall", "max_tokens", "ignored"))
}

func TestCompanionLegacyArgs_RecallMaxTokensMustBeANumber(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	before := legacyCount("recall", "max_tokens", "refused")
	for _, v := range []any{"lots", nil} {
		text, isErr := callTool(t, srv, raw, "recall", map[string]any{"query": "q", "max_tokens": v})
		require.Truef(t, isErr, "max_tokens %v: %s", v, text)
		assert.Contains(t, text, "max_tokens")
	}
	assert.Equal(t, before+2, legacyCount("recall", "max_tokens", "refused"))
}

// The 2026.10.5 agent benchmark's delegate names the key's own project: the
// project the delegation runs in anyway. Accepted, removed, counted.
func TestCompanionLegacyArgs_DelegateOwnProjectAccepted(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	before := legacyCount("delegate", "project", "ignored")
	text, isErr := callTool(t, srv, raw, "delegate", map[string]any{"workflow": "wf-alpha", "prompt": "p", "project": "alpha"})
	require.False(t, isErr, "a 2026.10.5 bench agent submit was refused: %s", text)
	require.Equal(t, 1, taskRepo.CallCount.Create)
	assert.Equal(t, "alpha", taskRepo.LastCall.Task.ProjectID)
	assert.Equal(t, before+1, legacyCount("delegate", "project", "ignored"))
}

// Any other project is refused before a task exists: the tolerance never
// routes a task somewhere the caller did not ask, and never silently runs it
// in the key's project while the caller named another (the #305 defect).
func TestCompanionLegacyArgs_DelegateOtherProjectRefused(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	before := legacyCount("delegate", "project", "refused")
	for _, p := range []any{"beta", "", 7} {
		text, isErr := callTool(t, srv, raw, "delegate", map[string]any{"workflow": "wf-alpha", "prompt": "p", "project": p})
		require.Truef(t, isErr, "project %v was accepted: %s", p, text)
		assert.Contains(t, text, `"alpha"`, "the refusal names the key's project")
	}
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	assert.Equal(t, before+3, legacyCount("delegate", "project", "refused"))
}

// An agent-admin key's delegation runs in the workflow's project, not the
// key's home, so no value of project is a no-op for it: always refused.
func TestCompanionLegacyArgs_DelegateProjectRefusedForAgentAdmin(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	keyRepo.rows[len(keyRepo.rows)-1].AgentAdmin = true
	keyRepo.rows[len(keyRepo.rows)-1].AgentNamespace = "alpha"

	before := legacyCount("delegate", "project", "refused")
	text, isErr := callTool(t, srv, raw, "delegate", map[string]any{"workflow": "wf-alpha", "prompt": "p", "project": "alpha"})
	require.True(t, isErr, text)
	assert.Contains(t, text, "project")
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	assert.Equal(t, before+1, legacyCount("delegate", "project", "refused"))
}

// The tolerance is not a back door to undeclared arguments in general: only
// the listed names, only on their tool.
func TestCompanionLegacyArgs_OnlyListedNames(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	text, isErr := callTool(t, srv, raw, "recall", map[string]any{"query": "q", "project": "alpha"})
	require.True(t, isErr, text)
	assert.Contains(t, text, `unknown argument "project"`)
	text, isErr = callTool(t, srv, raw, "recent_memory", map[string]any{"max_tokens": 10})
	require.True(t, isErr, text)
	assert.Contains(t, text, `unknown argument "max_tokens"`)
}

// A legacy name names a registered tool and is NOT declared by it (a
// declared name needs no tolerance, and tolerating it would bypass its
// type check).
func TestCompanionStrictArgs_LegacyArgsAreNotDeclared(t *testing.T) {
	require.Len(t, companionLegacyToolArgs, 2, "a legacy name is added only with the shipped client that sends it (LLD 21 §A, 2026-10-14)")
	for tool, names := range companionLegacyToolArgs {
		newArgs, ok := companionToolArgs[tool]
		require.Truef(t, ok, "legacy entry for unknown tool %s", tool)
		declared := map[string]bool{}
		for _, f := range jsonFieldNames(newArgs()) {
			declared[f] = true
		}
		for name := range names {
			assert.Falsef(t, declared[name], "%s.%s is declared; drop its legacy entry", tool, name)
		}
	}
}

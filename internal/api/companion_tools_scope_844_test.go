package api

// GitHub #844 (design 26 §3 C1): tools/list offers a tool exactly when the key's
// scopes let it call that tool. AWS DevOps Agent tells customers to allowlist
// read-only tools from the server's list; before this change a read-only key
// (--memory-read --no-delegate) was offered remember, memory_correct and every
// skill tool, and was refused only when it called them.
//
// Found at: review (design 26 as-built, 2026-11-19), observed at integration
// (tools/list over the real handler lists tools the key cannot call).

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
)

// scopeTools are the tools whose handlers check a memory or skill scope.
var scopeTools = []string{
	"recall", "recent_memory", "memory_list", "list_scopes", "remember", "memory_correct",
	"skill_search", "skill_get", "skill_list", "skill_propose", "skill_approve", "skill_reject", "skill_set_global",
}

// TestCompanionToolsScope844_OfferedExactlyWhenCallable checks, for every
// combination of the five scope flags, that each scope-checked tool is in
// tools/list exactly when its handler does not refuse it for scope.
func TestCompanionToolsScope844_OfferedExactlyWhenCallable(t *testing.T) {
	f := newDiagFixture(t)
	raw, _ := seedCompanionKey(t, f.keyRepo, "alpha", nil)
	row := f.keyRepo.rows[len(f.keyRepo.rows)-1] // the stored row the lookup reads
	for mask := 0; mask < 32; mask++ {
		row.MemoryRead, row.MemoryWrite = mask&1 != 0, mask&2 != 0
		row.SkillRead, row.SkillWrite, row.SkillAdmin = mask&4 != 0, mask&8 != 0, mask&16 != 0
		key, err := f.keyRepo.LookupActiveByHash(context.Background(), apikey.Hash(raw))
		require.NoError(t, err)
		offered := map[string]bool{}
		for _, d := range f.srv.companionToolsFor(key) {
			offered[d.Name] = true
		}
		for _, tool := range scopeTools {
			_, text, isErr := f.call(t, raw, tool, map[string]any{})
			refusedForScope := isErr && strings.Contains(text, "this key lacks")
			if offered[tool] == refusedForScope {
				t.Errorf("scopes %05b: %s offered=%v but refused for scope=%v (%s)", mask, tool, offered[tool], refusedForScope, text)
			}
		}
	}
}

// TestCompanionToolsScope844_ReadOnlyProfile pins the tools the public guide
// (docs/public/guides/aws-devops-agent.md) lists for its read-only profile:
// --memory-read --skill-read --no-delegate.
func TestCompanionToolsScope844_ReadOnlyProfile(t *testing.T) {
	f := newDiagFixture(t)
	_, row := seedCompanionKey(t, f.keyRepo, "alpha", nil)
	row.MemoryRead, row.SkillRead, row.DelegateDisabled = true, true, true
	row.MemoryWrite, row.SkillWrite, row.SkillAdmin = false, false, false
	var got []string
	for _, d := range f.srv.companionToolsFor(row) {
		got = append(got, d.Name)
	}
	require.ElementsMatch(t, []string{
		"whoami", "report_problem", "recall", "recent_memory", "memory_list", "list_scopes",
		"skill_search", "skill_get", "skill_list",
	}, got)
}

// TestCompanionToolsScope844_WriteBackProfile pins the guide's second profile:
// --memory-all --skill-read with delegation.
func TestCompanionToolsScope844_WriteBackProfile(t *testing.T) {
	f := newDiagFixture(t)
	_, row := seedCompanionKey(t, f.keyRepo, "alpha", nil)
	row.MemoryRead, row.MemoryWrite, row.SkillRead = true, true, true
	row.SkillWrite, row.SkillAdmin, row.DelegateDisabled = false, false, false
	var got []string
	for _, d := range f.srv.companionToolsFor(row) {
		got = append(got, d.Name)
	}
	require.ElementsMatch(t, []string{
		"whoami", "report_problem", "recall", "recent_memory", "memory_list", "list_scopes",
		"skill_search", "skill_get", "skill_list", "remember", "memory_correct",
		"delegate", "status", "result", "task_logs", "task_explain", "cancel", "list", "catalog",
		"list_artifacts", "read_artifact",
	}, got)
}

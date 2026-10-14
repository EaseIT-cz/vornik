package api

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #324 (audit 2026-10-12): an agent-admin key's broker delegations
// run in the project that owns the workflow (hermes--global-news), not its
// home project, and the Hermes digest called list {} — the home project
// only — so a finished delegation was never announced and the model kept
// reporting it RUNNING. list(project: "*") lists every project the key
// covers (LLD 21 §A, "list(project: \"*\")").

func TestCompanionList_StarListsEveryNamespaceProject(t *testing.T) {
	f := newListFixture(t)
	out, text, isErr := f.list(t, f.adminRaw, map[string]any{"project": "*"})
	require.False(t, isErr, text)
	assert.Equal(t, "*", out["project"])
	assert.Equal(t, 1, f.calls, "one round trip")
	listed := append([]string(nil), f.listed...)
	sort.Strings(listed)
	assert.Equal(t, []string{"hermes--home", "hermes--prague-venues"}, listed,
		"every loaded project of the key's namespace, and nothing of another namespace")
	assert.Contains(t, text, "task-of-hermes--prague-venues")
	assert.Contains(t, text, "task-of-hermes--home")
	assert.NotContains(t, text, "other--x")
	assert.NotContains(t, text, "task-of-alpha")
}

func TestCompanionList_StarIsBoundedByLimit(t *testing.T) {
	f := newListFixture(t)
	out, text, isErr := f.list(t, f.adminRaw, map[string]any{"project": "*", "limit": 1})
	require.False(t, isErr, text)
	tasks, _ := out["tasks"].([]any)
	assert.Len(t, tasks, 1)
}

// An empty ProjectIDs means "no constraint" to the repository; when the
// COVERED set is empty — here projects of other namespaces are loaded but the
// key's filter accepts none of them (review-20261012-a61a F1: the guard is on
// the filtered set, not on "anything loaded") — list answers empty without
// querying.
func TestCompanionList_StarNoProjectsListsNothing(t *testing.T) {
	f := newListFixture(t)
	raw, _ := seedCompanionKey(t, f.keyRepo, "ghost--home", nil)
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentAdmin = true
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentNamespace = "ghost"

	out, text, isErr := f.list(t, raw, map[string]any{"project": "*"})
	require.False(t, isErr, text)
	assert.Equal(t, 0, f.calls, "no List call may run with an empty project set")
	assert.EqualValues(t, 0, out["total"])
}

func TestCompanionList_StarPlainKeyIsOwnProject(t *testing.T) {
	f := newListFixture(t)
	out, text, isErr := f.list(t, f.plainRaw, map[string]any{"project": "*"})
	require.False(t, isErr, text)
	assert.Equal(t, "*", out["project"])
	assert.Equal(t, []string{"alpha"}, f.listed)
}

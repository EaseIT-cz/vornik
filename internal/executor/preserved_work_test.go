package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// audit 2026-10-12: a merge-back failure promised "worktree preserved for
// manual recovery", but the task-level retry's createWorktree removed that
// worktree and its branch (GitHub #354). On prod none of the 8 worktrees the
// messages named in 30 days still existed. Executor design §9.5.

// conflictingWorktree builds the S7b shape: a worktree branched off base, the
// operator advancing main on the same file, the agent committing its own
// version. Returns (repo, worktree).
func conflictingWorktree(t *testing.T, taskID string) (string, string) {
	t.Helper()
	requireGitExec(t)
	repo := seedRepo(t, t.TempDir(), "proj")
	wt, err := createWorktree(context.Background(), repo, taskID, zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "X"), []byte("operator\n"), 0o644))
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "operator push")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "X"), []byte("agent work\n"), 0o644))
	return repo, wt
}

func TestPreserveFailedWork_SurvivesRetryAndPrune(t *testing.T) {
	ctx := context.Background()
	taskID := "task_w3_9_survive"
	repo, wt := conflictingWorktree(t, taskID)
	require.Error(t, mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop()))

	now := time.Unix(1791840000, 0)
	pw, err := preserveFailedWork(ctx, repo, wt, taskID, 1, now, zerolog.Nop())
	require.NoError(t, err)
	require.NotEmpty(t, pw.SHA)
	assert.Equal(t, fmt.Sprintf("refs/vornik/preserved/%s/1-%d-%s", taskID, now.Unix(), pw.SHA[:12]), pw.Ref)

	// The next attempt, exactly as runExecution does it: createWorktree finds
	// the path taken, removeWorktree deletes the worktree and its branch, and
	// a fresh worktree is created. Then a daemon restart's prune.
	_, err = createWorktree(ctx, repo, taskID, zerolog.Nop())
	require.Error(t, err)
	removeWorktree(ctx, repo, worktreePath(repo, taskID), taskID, zerolog.Nop())
	_, err = createWorktree(ctx, repo, taskID, zerolog.Nop())
	require.NoError(t, err)
	pruneWorktrees(ctx, repo, zerolog.Nop(), removeUnless(nil))

	assert.Equal(t, "agent work", gitOut(t, repo, "git", "show", pw.Ref+":X"),
		"the preserved ref must still hold the failed attempt's work")
	listed, err := ListPreservedWork(repo)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, pw.Ref, listed[0].Ref)
	assert.Equal(t, pw.SHA, listed[0].SHA)
	assert.Equal(t, taskID, listed[0].TaskID)
	assert.Equal(t, now.Unix(), listed[0].PreservedAt.Unix())
}

// All 8 prod merge-backs failed with "not something we can merge": the branch
// was gone at merge time. The work is pinned from the worktree's HEAD.
func TestPreserveFailedWork_BranchMissingPinsWorktreeHEAD(t *testing.T) {
	ctx := context.Background()
	taskID := "task_w3_9_nobranch"
	repo, wt := conflictingWorktree(t, taskID)
	mustGit(t, wt, "git", "add", "-A")
	mustGit(t, wt, "git", "commit", "-q", "-m", "agent work")
	mustGit(t, wt, "git", "checkout", "-q", "--detach")
	mustGit(t, repo, "git", "branch", "-D", worktreeBranch(taskID))
	require.Error(t, mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop()))

	pw, err := preserveFailedWork(ctx, repo, wt, taskID, 2, time.Now(), zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, "agent work", gitOut(t, repo, "git", "show", pw.Ref+":X"))
}

func TestPreserveFailedWork_NothingToRecover(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	repo := seedRepo(t, t.TempDir(), "proj")
	taskID := "task_w3_9_empty"
	wt, err := createWorktree(ctx, repo, taskID, zerolog.Nop())
	require.NoError(t, err)

	pw, err := preserveFailedWork(ctx, repo, wt, taskID, 1, time.Now(), zerolog.Nop())
	require.NoError(t, err)
	assert.Empty(t, pw.Ref, "a worktree with no commits beyond HEAD has nothing to recover")
	listed, err := ListPreservedWork(repo)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestPrunePreservedWork_RetentionBound(t *testing.T) {
	ctx := context.Background()
	taskID := "task_w3_9_retention"
	repo, wt := conflictingWorktree(t, taskID)
	require.Error(t, mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop()))
	now := time.Now()
	old, err := preserveFailedWork(ctx, repo, wt, taskID, 1, now.Add(-PreservedWorkRetention-time.Hour), zerolog.Nop())
	require.NoError(t, err)
	young, err := preserveFailedWork(ctx, repo, wt, taskID, 2, now.Add(-time.Hour), zerolog.Nop())
	require.NoError(t, err)
	sha := strings.TrimSpace(gitOut(t, repo, "git", "rev-parse", young.Ref))
	unparseable := "refs/vornik/preserved/" + taskID + "/by-hand"
	mustGit(t, repo, "git", "update-ref", unparseable, sha)

	prunePreservedWork(ctx, repo, now, PreservedWorkRetention, zerolog.Nop())

	listed, err := ListPreservedWork(repo)
	require.NoError(t, err)
	var refs []string
	for _, p := range listed {
		refs = append(refs, p.Ref)
	}
	assert.NotContains(t, refs, old.Ref, "older than the retention bound: deleted")
	assert.Contains(t, refs, young.Ref)
	assert.Contains(t, refs, unparseable, "a name that does not parse is kept and listed")
}

// Packed refs (after `git pack-refs`, which gc runs) must still be listed: the
// doctor reads the files, it cannot run git.
func TestListPreservedWork_ReadsPackedRefs(t *testing.T) {
	ctx := context.Background()
	taskID := "task_w3_9_packed"
	repo, wt := conflictingWorktree(t, taskID)
	require.Error(t, mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop()))
	pw, err := preserveFailedWork(ctx, repo, wt, taskID, 1, time.Now(), zerolog.Nop())
	require.NoError(t, err)
	mustGit(t, repo, "git", "pack-refs", "--all")
	_, statErr := os.Stat(filepath.Join(repo, ".git", filepath.FromSlash(pw.Ref)))
	require.True(t, os.IsNotExist(statErr), "the ref must be packed for this test to mean anything")

	listed, err := ListPreservedWork(repo)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, pw.Ref, listed[0].Ref)
	assert.Equal(t, pw.SHA, listed[0].SHA)
}

// End to end through the executor: a merge-back conflict fails attempt 1 with
// MERGE_FAILED and a message naming the ref; attempt 2 (the task-level retry)
// replaces the worktree; the work is still recoverable from the ref.
func TestExecutor_MergeFailureThenRetry_WorkStillRecoverable(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	projectDir := filepath.Join(root, "p1")
	wt := worktreePath(projectDir, resumeTaskID)

	h := newResumeHarness(t,
		[]string{`{"status":"COMPLETED","message":"researched"}`, `{"status":"COMPLETED","message":"wrote"}`,
			`{"status":"COMPLETED","message":"researched"}`, `{"status":"COMPLETED","message":"wrote"}`},
		nil)
	h.e.config.ProjectWorkspacePath = root
	starts := 0
	h.rt.mu.Lock()
	h.rt.onStart = func(_ *runtime.ContainerConfig) {
		starts++
		if starts != 2 {
			return
		}
		// Attempt 1's write step: the agent edits README.md in its worktree
		// while the operator commits a divergent README.md on main.
		_ = os.WriteFile(filepath.Join(wt, "README.md"), []byte("agent attempt 1\n"), 0o644)
		_ = os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("operator\n"), 0o644)
		mustGit(t, projectDir, "git", "commit", "-q", "-am", "operator push")
	}
	h.rt.mu.Unlock()

	h.run(t)
	execs := h.executions(t)
	require.Len(t, execs, 1)
	require.NotNil(t, execs[0].ErrorCode)
	assert.Equal(t, persistence.TaskFailureClassMergeFailed, *execs[0].ErrorCode)
	require.NotNil(t, execs[0].ErrorMessage)
	msg := *execs[0].ErrorMessage
	listed, err := ListPreservedWork(projectDir)
	require.NoError(t, err)
	require.Len(t, listed, 1, "the merge failure must pin the work")
	ref := listed[0].Ref
	assert.Contains(t, msg, ref, "the message must name the ref the work is pinned at")
	assert.NotContains(t, msg, "for manual recovery", "the old promise is gone")

	h.requeue(t)
	h.run(t)

	assert.Equal(t, "agent attempt 1", gitOut(t, projectDir, "git", "show", ref+":README.md"),
		"after the retry replaced the worktree, the first attempt's work is still recoverable")
}

// review-20261012-4a89 F2: two failures of one attempt in one second (an
// operator retry spends no attempt) must not collide: different work gets a
// different ref, and pinning the same commit twice is not an error.
func TestPreserveFailedWork_SameSecondDoesNotCollide(t *testing.T) {
	ctx := context.Background()
	taskID := "task_w3_9_collide"
	repo, wt := conflictingWorktree(t, taskID)
	require.Error(t, mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop()))
	now := time.Now()
	first, err := preserveFailedWork(ctx, repo, wt, taskID, 1, now, zerolog.Nop())
	require.NoError(t, err)
	again, err := preserveFailedWork(ctx, repo, wt, taskID, 1, now, zerolog.Nop())
	require.NoError(t, err, "re-pinning the same commit is the same pin")
	assert.Equal(t, first.Ref, again.Ref)

	require.NoError(t, os.WriteFile(filepath.Join(wt, "X"), []byte("agent work, second try\n"), 0o644))
	mustGit(t, wt, "git", "commit", "-q", "-am", "second try")
	second, err := preserveFailedWork(ctx, repo, wt, taskID, 1, now, zerolog.Nop())
	require.NoError(t, err)
	assert.NotEqual(t, first.Ref, second.Ref)
	assert.Equal(t, "agent work", gitOut(t, repo, "git", "show", first.Ref+":X"))
	assert.Equal(t, "agent work, second try", gitOut(t, repo, "git", "show", second.Ref+":X"))
}

// review-20261012-4a89 F1 asked whether the iteration-limit path pins too. It
// calls the same cleanupWorktree(true), so it does: the partial work is pinned,
// the task error names the ref, and the class stays TOOL_ITERATION_LIMIT.
func TestExecutor_IterationLimitMergeFailure_PinsAndKeepsClass(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	projectDir := filepath.Join(root, "p1")
	wt := worktreePath(projectDir, resumeTaskID)

	h := newResumeHarness(t,
		[]string{`{"status":"COMPLETED","message":"researched"}`,
			`{"status":"FAILED","message":"Tool iteration limit reached (50) before a final answer"}`},
		nil)
	h.e.config.ProjectWorkspacePath = root
	task, err := h.tr.Get(context.Background(), resumeTaskID)
	require.NoError(t, err)
	task.MaxAttempts = 1
	require.NoError(t, h.tr.Update(context.Background(), task))
	starts := 0
	h.rt.mu.Lock()
	h.rt.onStart = func(_ *runtime.ContainerConfig) {
		starts++
		if starts != 2 {
			return
		}
		_ = os.WriteFile(filepath.Join(wt, "README.md"), []byte("partial work\n"), 0o644)
		_ = os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("operator\n"), 0o644)
		mustGit(t, projectDir, "git", "commit", "-q", "-am", "operator push")
	}
	h.rt.mu.Unlock()

	h.run(t)
	execs := h.executions(t)
	require.Len(t, execs, 1)
	require.NotNil(t, execs[0].ErrorCode)
	assert.Equal(t, persistence.TaskFailureClassToolIterationLimit, *execs[0].ErrorCode)
	listed, err := ListPreservedWork(projectDir)
	require.NoError(t, err)
	require.Len(t, listed, 1, "the failed checkpoint merge must pin the partial work")
	assert.Equal(t, "partial work", gitOut(t, projectDir, "git", "show", listed[0].Ref+":README.md"))
	require.NotNil(t, execs[0].ErrorMessage)
	assert.Contains(t, *execs[0].ErrorMessage, listed[0].Ref)
}

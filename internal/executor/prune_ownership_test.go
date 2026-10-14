package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12 (GitHub #357): all 8 prod MERGE_FAILED rows of the 30 days
// to 2026-10-12 lost worktree/<task> before the merge-back. A second daemon
// sharing the workspace root (the spawnlaw e2e daemon, which inherited the
// shell's VORNIK_DATA_DIR) ran its startup prune with its OWN database: its
// preserve set was empty and every live prod worktree was removed. Executor
// design §9.5.4–§9.5.5.

// removeUnless is the pre-#357 rule as a decider, for tests that pin the
// prune's mechanics rather than its ownership: remove everything not in keep.
func removeUnless(keep map[string]struct{}) pruneDecider {
	return func(_ context.Context, taskID string) (bool, string) {
		if _, ok := keep[taskID]; ok {
			return false, "kept by test"
		}
		return true, "test"
	}
}

func w43BranchPresent(t *testing.T, repo, taskID string) bool {
	t.Helper()
	out, err := gitExec.output(context.Background(), "-C", repo, "branch", "--list", worktreeBranch(taskID))
	require.NoError(t, err)
	return strings.TrimSpace(string(out)) != ""
}

func w43DirPresent(repo, taskID string) bool {
	_, err := os.Stat(worktreePath(repo, taskID))
	return err == nil
}

// A daemon whose database does not know a task must not remove its worktree:
// this is the foreign-prune shape, reproduced through Recover itself.
func TestRecover_ForeignDaemonKeepsWorktreesItsDatabaseDoesNotKnow(t *testing.T) {
	requireGitExec(t)
	root := t.TempDir()
	repo := seedRepo(t, root, "proj")
	const live = "task_prod_live_merge_back"
	_, err := createWorktree(context.Background(), repo, live, zerolog.Nop())
	require.NoError(t, err)

	e, _, _, _, _ := setup() // empty task + execution repos: a foreign database
	e.config.ProjectWorkspacePath = root
	require.NoError(t, e.Recover(context.Background()))

	assert.True(t, w43BranchPresent(t, repo, live), "a foreign daemon's startup prune deleted a live task's branch")
	assert.True(t, w43DirPresent(repo, live), "a foreign daemon's startup prune deleted a live task's worktree directory")
}

func TestStartupPruneDecider_RemovesOnlyOwnTerminalTasks(t *testing.T) {
	e, _, _, _, tr := setup()
	for id, st := range map[string]persistence.TaskStatus{
		"t_completed": persistence.TaskStatusCompleted,
		"t_failed":    persistence.TaskStatusFailed,
		"t_cancelled": persistence.TaskStatusCancelled,
		"t_closed":    persistence.TaskStatusClosed,
		"t_queued":    persistence.TaskStatusQueued,
		"t_running":   persistence.TaskStatusRunning,
		"t_inflight":  persistence.TaskStatusFailed,
	} {
		tr.AddTask(&persistence.Task{ID: id, ProjectID: "p", Status: st, CreatedAt: time.Now()})
	}
	decide := e.startupPruneDecider(map[string]struct{}{"t_inflight": {}})
	ctx := context.Background()
	for id, want := range map[string]bool{
		"t_completed": true, "t_failed": true, "t_cancelled": true, "t_closed": true,
		"t_queued": false, "t_running": false, "t_inflight": false, "t_unknown": false,
	} {
		got, reason := decide(ctx, id)
		assert.Equal(t, want, got, "%s (%s)", id, reason)
		assert.NotEmpty(t, reason, id)
	}

	tr.err = errors.New("db down")
	got, _ := decide(ctx, "t_completed")
	assert.False(t, got, "a lookup error must keep the worktree")
}

// A nil decider removes nothing: the zero value is the safe one.
func TestPruneWorktrees_NilDeciderRemovesNothing(t *testing.T) {
	requireGitExec(t)
	repo := seedRepo(t, t.TempDir(), "proj")
	_, err := createWorktree(context.Background(), repo, "task_a", zerolog.Nop())
	require.NoError(t, err)
	pruneWorktrees(context.Background(), repo, zerolog.Nop(), nil)
	assert.True(t, w43BranchPresent(t, repo, "task_a"))
	assert.True(t, w43DirPresent(repo, "task_a"))
}

// The second pass removed any .worktrees/<id> without a branch. After a
// foreign prune deleted the branch, that is the live directory podman is
// about to bind-mount (the 2026-09-15 statfs row).
func TestPruneOrphanWorktreeDirs_KeepsDirOfUnknownTask(t *testing.T) {
	requireGitExec(t)
	repo := seedRepo(t, t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".worktrees", "task_unknown"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".worktrees", "task_done"), 0o755))
	decide := func(_ context.Context, id string) (bool, string) { return id == "task_done", "test" }
	pruneOrphanWorktreeDirs(context.Background(), repo, zerolog.Nop(), decide)
	assert.True(t, w43DirPresent(repo, "task_unknown"))
	assert.False(t, w43DirPresent(repo, "task_done"))
}

// The in-use guard asked podman for mount DESTINATIONS and compared a host
// path: it could never match. It now asks by the task label.
func TestWorktreeInUseByContainer_AsksByTaskLabel(t *testing.T) {
	var got []string
	orig := podmanPSRunning
	podmanPSRunning = func(_ context.Context, argv []string) (string, error) {
		got = argv
		return "abc123\n", nil
	}
	t.Cleanup(func() { podmanPSRunning = orig })

	assert.True(t, worktreeInUseByContainer(context.Background(), "task_x"))
	assert.Equal(t, []string{"ps", "--no-trunc", "--filter", "status=running",
		"--filter", "label=vornik.taskId=task_x", "--format", "{{.ID}}"}, got)

	podmanPSRunning = func(context.Context, []string) (string, error) { return "", nil }
	assert.False(t, worktreeInUseByContainer(context.Background(), "task_x"))
	podmanPSRunning = func(context.Context, []string) (string, error) { return "", errors.New("no podman") }
	assert.False(t, worktreeInUseByContainer(context.Background(), "task_x"), "fail-open")
	assert.False(t, worktreeInUseByContainer(context.Background(), ""))
}

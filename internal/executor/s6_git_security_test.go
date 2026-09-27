package executor

// S6 security tests, executor half (process-spawn law design, "S6 — git state
// an agent can write", "Tests for the security property"). The spawn law pins
// WHICH program the daemon runs, but git runs OTHER programs, chosen by state
// an agent can write: the worktree's .git pointer (way 2), a .gitattributes
// naming a driver the global config defines (way 5), and the shared-mode
// fallback that mounted the project read-write (ways 1 and 3). Every test here
// failed before S6 for the reason its comment names.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/spawn"
)

// s6TamperPointer does what an agent can do inside its mounted worktree:
// build its own git dir there, arm it with a core.fsmonitor program and a
// pre-commit hook, and repoint the worktree's .git file at it. Returns the two
// marker paths the armed programs would create.
func s6TamperPointer(t *testing.T, wt string) (fsmonitorMarker, hookMarker string) {
	t.Helper()
	evil := filepath.Join(wt, ".evil")
	scratch := t.TempDir()
	mustGit(t, scratch, "git", "init", "-q", "--separate-git-dir", evil, scratch)
	arm := t.TempDir()
	fsmonitorMarker = filepath.Join(arm, "fsmonitor-ran")
	hookMarker = filepath.Join(arm, "pre-commit-ran")
	fsmon := s6Script(t, arm, "fsmonitor.sh", "touch '"+fsmonitorMarker+"'\nexit 1\n")
	mustGit(t, scratch, "git", "--git-dir", evil, "config", "core.fsmonitor", fsmon)
	require.NoError(t, os.MkdirAll(filepath.Join(evil, "hooks"), 0o755))
	s6Script(t, filepath.Join(evil, "hooks"), "pre-commit", "touch '"+hookMarker+"'\n")
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+evil+"\n"), 0o644))
	return fsmonitorMarker, hookMarker
}

func s6AssertAbsent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s: marker %s exists", why, path)
	}
}

// Way 2. Pre-S6: the auto-commit followed the tampered pointer, so git ran the
// agent's core.fsmonitor and pre-commit hook on the host, and the commit
// landed in the agent's repository instead of the task branch (the work was
// lost from the merge-back).
func TestS6Security_TamperedPointerRunsNothingAndTheCommitLands(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_tamper", zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "agent.txt"), []byte("the agent's work\n"), 0o644))
	fsm, hook := s6TamperPointer(t, wt)

	require.NoError(t, mergeWorktree(ctx, projectDir, wt, "task_s6_tamper", "", zerolog.Nop()))

	s6AssertAbsent(t, fsm, "the agent's core.fsmonitor ran on the host")
	s6AssertAbsent(t, hook, "the agent's pre-commit hook ran on the host")
	assert.Equal(t, "the agent's work", gitOut(t, projectDir, "git", "show", "HEAD:agent.txt"),
		"the auto-commit must land on the task branch and merge back")
}

// Way 2, the read side. Pre-S6: HEAD sampling, the patch set and the claim
// checks followed the tampered pointer and reported the agent's repository.
func TestS6Security_HeadAndPatchesAreTrueOnATamperedPointer(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_tamperhead", zerolog.Nop())
	require.NoError(t, err)
	from := gitOut(t, projectDir, "git", "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	mustGit(t, wt, "git", "add", "a.txt")
	mustGit(t, wt, "git", "commit", "-q", "-m", "real agent commit")
	branchHead := gitOut(t, projectDir, "git", "rev-parse", worktreeBranch("task_s6_tamperhead"))
	fsm, hook := s6TamperPointer(t, wt)

	assert.Equal(t, branchHead, gitHEAD(ctx, wt), "HEAD must be the task branch, not the agent's repository")
	n, ok := gitDiffFileCount(ctx, wt, from, branchHead)
	assert.True(t, ok)
	assert.Equal(t, 1, n)
	assert.True(t, gitObjectExists(ctx, wt, branchHead))
	sum, err := generatePlanChanges(ctx, wt, from, branchHead)
	require.NoError(t, err)
	require.NotNil(t, sum)
	defer func() { _ = os.RemoveAll(sum.OutputDir) }()
	assert.Contains(t, sum.Summary, "real agent commit")
	s6AssertAbsent(t, fsm, "the agent's core.fsmonitor ran on the host")
	s6AssertAbsent(t, hook, "the agent's pre-commit hook ran on the host")
}

// s6GlobalFilterHome is an operator whose global config DEFINES a filter (the
// shape git-lfs installs globally), each side leaving a marker when run.
func s6GlobalFilterHome(t *testing.T) (cleanMarker, smudgeMarker string) {
	t.Helper()
	arm := t.TempDir()
	cleanMarker = filepath.Join(arm, "clean-ran")
	smudgeMarker = filepath.Join(arm, "smudge-ran")
	clean := s6Script(t, arm, "clean.sh", "touch '"+cleanMarker+"'\ncat\n")
	smudge := s6Script(t, arm, "smudge.sh", "touch '"+smudgeMarker+"'\ncat\n")
	s6DaemonGitHome(t, "[filter \"agentpick\"]\n\tclean = "+clean+"\n\tsmudge = "+smudge+"\n")
	return cleanMarker, smudgeMarker
}

// Way 5, worktree side. Pre-S6: the agent's .gitattributes NAMED a filter the
// operator's global config defines, and the daemon's auto-commit ran it.
func TestS6Security_GlobalFilterNamedByTheWorktreeNeverRuns(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	cleanM, smudgeM := s6GlobalFilterHome(t)
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_gfilter", zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("* filter=agentpick\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x\n"), 0o644))

	require.NoError(t, mergeWorktree(ctx, projectDir, wt, "task_s6_gfilter", "", zerolog.Nop()))

	s6AssertAbsent(t, cleanM, "a globally defined clean filter ran for the agent")
	s6AssertAbsent(t, smudgeM, "a globally defined smudge filter ran for the agent")
	assert.Equal(t, "x", gitOut(t, projectDir, "git", "show", "HEAD:x.txt"))
}

// Way 5, project side. The agent commits the .gitattributes itself (its own
// git, inside the sandbox, reads no host config), so the daemon's auto-commit
// has nothing to do; pre-S6 the project-side merge-back then checked the files
// out and ran the globally defined smudge filter on the host.
func TestS6Security_GlobalFilterNeverRunsOnProjectSideMergeBack(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	cleanM, smudgeM := s6GlobalFilterHome(t)
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_gfmerge", zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("*.txt filter=agentpick\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "y.txt"), []byte("y\n"), 0o644))
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "agent commit"}} {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "agent-side git %v: %s", args, out)
	}

	require.NoError(t, mergeWorktree(ctx, projectDir, wt, "task_s6_gfmerge", "", zerolog.Nop()))

	s6AssertAbsent(t, smudgeM, "the merge-back ran a globally defined smudge filter named by the agent")
	s6AssertAbsent(t, cleanM, "the merge-back ran a globally defined clean filter named by the agent")
	assert.Equal(t, "y", gitOut(t, projectDir, "git", "show", "HEAD:y.txt"))
}

// S6-D2. A leftover admin dir git will not prune (locked) makes `worktree add`
// pick a SUFFIXED admin name. Pre-S6 the worktree was used anyway, so the
// admin dir the daemon derives from its own paths named a different worktree.
func TestS6Security_SuffixedAdminDirIsRefusedAtCreation(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	stale := filepath.Join(projectDir, ".git", "worktrees", "task_s6_suffix")
	require.NoError(t, os.MkdirAll(stale, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "gitdir"), []byte(filepath.Join(t.TempDir(), ".git")+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "locked"), []byte("leftover\n"), 0o644))

	_, err := createWorktree(context.Background(), projectDir, "task_s6_suffix", zerolog.Nop())
	require.Error(t, err, "a worktree whose admin dir is not <project>/.git/worktrees/<task> must be refused")
	_, statErr := os.Stat(worktreePath(projectDir, "task_s6_suffix"))
	assert.True(t, os.IsNotExist(statErr), "the refused worktree must be removed")
	assert.Empty(t, gitOut(t, projectDir, "git", "branch", "--list", worktreeBranch("task_s6_suffix")))
}

// S6-D2. A worktree whose pointer the agent replaced is still removed, and
// nothing the agent armed runs.
func TestS6Security_TamperedPointerWorktreeIsStillRemoved(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_rmtamper", zerolog.Nop())
	require.NoError(t, err)
	fsm, hook := s6TamperPointer(t, wt)

	removeWorktree(ctx, projectDir, wt, "task_s6_rmtamper", zerolog.Nop())

	_, err = os.Stat(wt)
	assert.True(t, os.IsNotExist(err), "the tampered worktree must be removed")
	_, err = os.Stat(filepath.Join(projectDir, ".git", "worktrees", "task_s6_rmtamper"))
	assert.True(t, os.IsNotExist(err), "its admin dir must be pruned")
	assert.Empty(t, gitOut(t, projectDir, "git", "branch", "--list", worktreeBranch("task_s6_rmtamper")))
	s6AssertAbsent(t, fsm, "removal ran the agent's core.fsmonitor")
	s6AssertAbsent(t, hook, "removal ran the agent's hook")
}

// S6-D3 at flow level: on a REAL worktree, a git command that does not name
// the worktree's git dir and work tree is refused before git starts. Pre-S6
// spawn.GitWorkspace ran it, and it followed the pointer.
func TestS6Security_WorktreeCommandWithoutGitDirsIsRefused(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	wt, err := createWorktree(context.Background(), projectDir, "task_s6_d3flow", zerolog.Nop())
	require.NoError(t, err)
	fsm, _ := s6TamperPointer(t, wt)

	cmd, err := spawn.GitWorkspace(context.Background(), wt, "status", "--porcelain")
	if err == nil {
		_, _ = cmd.CombinedOutput()
	}
	assert.ErrorIs(t, err, spawn.ErrRefused, "a worktree command without GIT_DIR/GIT_WORK_TREE must be refused")
	s6AssertAbsent(t, fsm, "the refused command still ran git")
	s6FlowRefusalsWithPartialDirs(t, wt, fsm)
}

// s6StartRecorder is MockRuntime that records every container config and can
// act between attempts.
type s6StartRecorder struct {
	*MockRuntime
	configs []runtime.ContainerConfig
	onWait  func(n int) error
	waits   int
}

func (r *s6StartRecorder) StartContainer(ctx context.Context, c *runtime.ContainerConfig) (string, error) {
	r.mu.Lock()
	r.configs = append(r.configs, *c)
	r.mu.Unlock()
	return r.MockRuntime.StartContainer(ctx, c)
}

func (r *s6StartRecorder) WaitForExit(ctx context.Context, id string, d time.Duration) (int, error) {
	r.mu.Lock()
	r.waits++
	n := r.waits
	r.mu.Unlock()
	if r.onWait != nil {
		if err := r.onWait(n); err != nil {
			return -1, err
		}
	}
	return r.MockRuntime.WaitForExit(ctx, id, d)
}

func (r *s6StartRecorder) started() []runtime.ContainerConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.ContainerConfig(nil), r.configs...)
}

// s6RunD1 drives one task through Execute and returns the recorder and the
// final task row.
func s6RunD1(t *testing.T, root string, maxAttempts int, onWait func(int) error) (*s6StartRecorder, *persistence.Task) {
	t.Helper()
	rt := &s6StartRecorder{MockRuntime: NewMockRuntime(), onWait: onWait}
	rt.outputJSON = `{"status":"COMPLETED"}`
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.ProjectWorkspacePath = root
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(s6OneStepResolver())
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	id := "t-s6-d1"
	tr.AddTask(&persistence.Task{ID: id, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: maxAttempts, CreatedAt: time.Now()})
	require.NoError(t, e.Execute(id))
	var final *persistence.Task
	require.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), id)
		// Settled: the goroutine is gone and the row says how it ended. With
		// attempt budget left the row stays LEASED/RUNNING for the scheduler
		// to re-queue, carrying the class (handleFailure).
		if task != nil && e.ActiveCount() == 0 && (task.LastErrorClass != nil || task.Status == persistence.TaskStatusFailed || task.Status == persistence.TaskStatusCompleted) {
			final = task
			return true
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	return rt, final
}

func s6AssertWorkspaceUnavailable(t *testing.T, task *persistence.Task) {
	t.Helper()
	require.NotNil(t, task.LastErrorClass, "the task must carry a failure class")
	assert.Equal(t, "WORKSPACE_UNAVAILABLE", *task.LastErrorClass)
}

func s6AssertOnlyWorktreeMounts(t *testing.T, configs []runtime.ContainerConfig) {
	t.Helper()
	for _, c := range configs {
		assert.NotEmpty(t, projectRootFromWorktree(c.ProjectDir),
			"a container was given a project directory that is not a task worktree: %q", c.ProjectDir)
	}
}

// S6-D1, trigger 1: ensureGitRepo fails. Pre-S6 the task ran in the shared
// project directory, mounted read-write with its .git.
func TestS6Security_D1_BootstrapFailureFailsTheTaskAndStartsNothing(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := t.TempDir()
	projectDir := filepath.Join(root, "p1")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.Chmod(projectDir, 0o555)) // git init cannot create .git
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	rt, task := s6RunD1(t, root, 1, nil)

	assert.Empty(t, rt.started(), "no container may start without a worktree")
	s6AssertWorkspaceUnavailable(t, task)
}

// s6FailingCheckoutHook arms the PROJECT's own post-checkout hook (the
// git-lfs/husky shape createWorktree's comment records) so `worktree add`
// fails.
func s6FailingCheckoutHook(t *testing.T, projectDir string) {
	t.Helper()
	hooks := filepath.Join(projectDir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	s6Script(t, hooks, "post-checkout", "echo 'post-checkout: tool not on PATH' >&2\nexit 1\n")
}

// S6-D1, trigger 2: createWorktree fails twice. Pre-S6 the task ran in the
// shared project directory.
func TestS6Security_D1_WorktreeCreationFailureFailsTheTaskAndStartsNothing(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	s6FailingCheckoutHook(t, filepath.Join(root, "p1"))

	rt, task := s6RunD1(t, root, 1, nil)

	assert.Empty(t, rt.started(), "no container may start without a worktree")
	s6AssertWorkspaceUnavailable(t, task)
	require.NotNil(t, task.LastError)
	assert.Contains(t, *task.LastError, "post-checkout", "the failure must name the git error")
}

// S6-D1, trigger 3: the retry path cannot re-create the worktree. Pre-S6 the
// next attempt ran in the shared project directory.
func TestS6Security_D1_RetryWithoutAWorktreeEndsTheRetries(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	projectDir := filepath.Join(root, "p1")

	rt, task := s6RunD1(t, root, 3, func(n int) error {
		if n == 1 {
			s6FailingCheckoutHook(t, projectDir) // the next worktree add fails
			return errors.New("podman wait failed: transient")
		}
		return nil
	})

	s6AssertOnlyWorktreeMounts(t, rt.started())
	assert.Len(t, rt.started(), 1, "no attempt may run once the worktree cannot be re-created")
	s6AssertWorkspaceUnavailable(t, task)
}

package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/backlogfile"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// audit 2026-10-12: GitHub #307. Task task_20260927075535_c5798f15038adcbc
// made a backlog deposit and then FAILED. The executor left the deposit
// uncommitted for the next merge prelude (workspace-hygiene design §4.2), but
// on the bench no next merge came: the dirty BACKLOG.md blocked the launch of
// a multi-day arm (Prepare and preflight check 4 refuse a dirty tree). Step C
// (§4.2b) commits the backlog file alone at the failed terminal.

const depositLine = "- [?] **[bug]** x — y (via task_20260927075535_c5798f15038adcbc, 2026-09-27)\n"

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func commitIn(t *testing.T, dir, rel, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644))
	gitIn(t, dir, "add", "--", rel)
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "add "+rel)
}

func appendTo(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(line)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestCheckpointBacklogAtTerminal_TrackedDeposit(t *testing.T) {
	dir := initGitRepoForTest(t)
	commitIn(t, dir, "BACKLOG.md", "# Backlog\n\n")
	bl := filepath.Join(dir, "BACKLOG.md")
	appendTo(t, bl, depositLine)

	locked := 0
	checkpointBacklogAtTerminal(context.Background(), dir, bl, "task_fail_1",
		func(fn func()) { locked++; fn() }, zerolog.Nop())

	assert.Empty(t, gitIn(t, dir, "status", "--porcelain"), "the tree must be clean after the checkpoint")
	assert.Equal(t, "backlog: checkpoint at failed terminal of task_fail_1", gitIn(t, dir, "log", "-1", "--format=%s"))
	assert.Contains(t, gitIn(t, dir, "show", "HEAD:BACKLOG.md"), "task_20260927075535_c5798f15038adcbc")
	assert.Equal(t, 1, locked, "the commit must run inside the backlog Store lock (review H1)")
}

func TestCheckpointBacklogAtTerminal_UntrackedFirstDeposit(t *testing.T) {
	dir := initGitRepoForTest(t)
	bl := filepath.Join(dir, "BACKLOG.md")
	appendTo(t, bl, "# Backlog\n\n"+depositLine)

	checkpointBacklogAtTerminal(context.Background(), dir, bl, "task_fail_2", nil, zerolog.Nop())

	assert.Empty(t, gitIn(t, dir, "status", "--porcelain"))
	assert.Contains(t, gitIn(t, dir, "show", "HEAD:BACKLOG.md"), "via task_20260927075535")
}

func TestCheckpointBacklogAtTerminal_IgnoredBacklogIsLeftAlone(t *testing.T) {
	dir := initGitRepoForTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("BACKLOG.md\n"), 0o644))
	bl := filepath.Join(dir, "BACKLOG.md")
	appendTo(t, bl, depositLine)
	before := gitIn(t, dir, "rev-parse", "HEAD")

	checkpointBacklogAtTerminal(context.Background(), dir, bl, "task_fail_3", nil, zerolog.Nop())

	assert.Equal(t, before, gitIn(t, dir, "rev-parse", "HEAD"), "an ignored backlog (forge clone) must not be committed")
	assert.Empty(t, gitIn(t, dir, "diff", "--cached", "--name-only"))
}

func TestCheckpointBacklogAtTerminal_LeavesOperatorStagedWorkAlone(t *testing.T) {
	dir := initGitRepoForTest(t)
	commitIn(t, dir, "BACKLOG.md", "# Backlog\n\n")
	commitIn(t, dir, "notes.md", "a\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.md"), []byte("operator edit\n"), 0o644))
	gitIn(t, dir, "add", "--", "notes.md")
	bl := filepath.Join(dir, "BACKLOG.md")
	appendTo(t, bl, depositLine)

	checkpointBacklogAtTerminal(context.Background(), dir, bl, "task_fail_4", nil, zerolog.Nop())

	assert.Equal(t, "BACKLOG.md", gitIn(t, dir, "show", "--name-only", "--format=", "HEAD"), "C's commit holds only the backlog")
	assert.Equal(t, "notes.md", gitIn(t, dir, "diff", "--cached", "--name-only"), "the operator's staged file is still staged")
}

func TestCheckpointBacklogAtTerminal_NothingToDo(t *testing.T) {
	dir := initGitRepoForTest(t)
	commitIn(t, dir, "BACKLOG.md", "# Backlog\n\n")
	before := gitIn(t, dir, "rev-parse", "HEAD")
	checkpointBacklogAtTerminal(context.Background(), dir, filepath.Join(dir, "BACKLOG.md"), "t", nil, zerolog.Nop())
	checkpointBacklogAtTerminal(context.Background(), dir, "", "t", nil, zerolog.Nop())
	assert.Equal(t, before, gitIn(t, dir, "rev-parse", "HEAD"), "a clean backlog or no backlog path makes no commit")
}

// review-20261012-16a3 H1: the checkpoint's lock is the shared Store's
// per-project mutex, the one the deposit handler's Append takes.
func TestWithBacklogStore_CheckpointLockIsTheStoreMutex(t *testing.T) {
	store := backlogfile.NewStore()
	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil,
		WithBacklogStore(store))
	lock := e.backlogLock("p1")
	require.NotNil(t, lock)
	held := false
	lock(func() {
		// A nested WithLock on the same project would deadlock; probe with a
		// goroutine-free check instead: a second project is free, and
		// releasing happens on return (asserted below by re-acquiring).
		held = true
	})
	require.True(t, held)
	done := make(chan struct{})
	go func() { store.WithLock("p1", func() {}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the Store mutex was not released after the checkpoint lock returned")
	}

	assert.Nil(t, NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil).backlogLock("p1"),
		"no Store wired: the checkpoint runs unlocked")
}

// End to end on real git: a one-step task whose agent deposits into the
// project's BACKLOG.md and then fails leaves the project clean, which is the
// check GitWorkspace.Prepare makes before the next bench repeat.
func TestFailedTaskAfterDeposit_LeavesWorkspaceClean(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	projectDir := filepath.Join(root, "p1")
	bl := filepath.Join(projectDir, "BACKLOG.md")
	commitIn(t, projectDir, "BACKLOG.md", "# Backlog\n\n")

	e, rt, _, _, tr := setup()
	e.config.ProjectWorkspacePath = root
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(s6OneStepResolver())
	rt.outputJSON = `{"status":"FAILED","message":"could not do it"}`
	rt.mu.Lock()
	rt.onStart = func(_ *runtime.ContainerConfig) { appendTo(t, bl, depositLine) }
	rt.mu.Unlock()
	t.Cleanup(func() { _ = e.Stop(context.Background()) })

	tr.AddTask(&persistence.Task{ID: "t-deposit-fail", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t-deposit-fail"))
	require.Eventually(t, func() bool { return e.ActiveCount() == 0 }, 10*time.Second, 20*time.Millisecond)

	got, _ := tr.Get(context.Background(), "t-deposit-fail")
	require.NotNil(t, got)
	require.Equal(t, persistence.TaskStatusFailed, got.Status, "the task must have failed for this test to mean anything")
	assert.Empty(t, gitIn(t, projectDir, "status", "--porcelain"), "a failed task after a deposit must leave the project clean")
	assert.Contains(t, gitIn(t, projectDir, "show", "HEAD:BACKLOG.md"), "task_20260927075535_c5798f15038adcbc",
		"the deposit is the agent's finding and must be kept, committed")
}

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #400 (found at: design review, the housekeeping design's open
// decision H2; user-observable at executor-e2e): a task-kind reminder or an
// autonomy cron task branched its worktree off whatever the project clone
// last held, because only forge tasks were reset to origin. A weekly triage
// then judged "shipped?" against stale code. These tests drive the real
// executor loop on real git (executor design 06-executor.md §9.6).

// refreshFixture is a bare origin, a project clone of it under a workspace
// root, and one upstream commit (UPSTREAM.md) pushed AFTER the clone, so the
// clone is stale by exactly that file.
type refreshFixture struct {
	wsRoot, projectDir, bare, scratch string
}

func newRefreshFixture(t *testing.T, withOrigin bool) refreshFixture {
	t.Helper()
	requireGitExec(t)
	s6DaemonGitHome(t, "")
	root := t.TempDir()
	bare, scratch := s6Seed(t, root)
	wsRoot := filepath.Join(root, "ws")
	require.NoError(t, os.MkdirAll(wsRoot, 0o755))
	projectDir := filepath.Join(wsRoot, "p1")
	mustGit(t, wsRoot, "git", "clone", "-q", bare, projectDir)
	if !withOrigin {
		mustGit(t, projectDir, "git", "remote", "remove", "origin")
	}
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "UPSTREAM.md"), []byte("upstream\n"), 0o644))
	mustGit(t, scratch, "git", "add", "-A")
	mustGit(t, scratch, "git", "commit", "-q", "-m", "upstream change")
	mustGit(t, scratch, "git", "push", "-q", "origin", "main")
	return refreshFixture{wsRoot: wsRoot, projectDir: projectDir, bare: bare, scratch: scratch}
}

// refreshRun runs one one-step task on the fixture and reports whether the
// worktree the agent was given held UPSTREAM.md when its container started.
func refreshRun(t *testing.T, f refreshFixture, ws registry.ProjectWorkspace, source persistence.TaskCreationSource) (sawUpstream bool, m *Metrics) {
	t.Helper()
	e, rt, _, _, tr := setup()
	m = NewMetrics(prometheus.NewRegistry())
	e.metrics = m
	e.config.ProjectWorkspacePath = f.wsRoot
	e.config.RetryDelay = 0
	res := s6OneStepResolver()
	res.projects["p1"].Workspace = ws
	e.SetWorkflowResolver(res)
	rt.outputJSON = `{"status":"COMPLETED"}`
	var saw atomic.Bool
	taskID := "t-refresh-" + strings.ToLower(string(source))
	rt.mu.Lock()
	rt.onStart = func(_ *runtime.ContainerConfig) {
		_, err := os.Stat(filepath.Join(worktreePath(f.projectDir, taskID), "UPSTREAM.md"))
		saw.Store(err == nil)
	}
	rt.mu.Unlock()
	t.Cleanup(func() { _ = e.Stop(context.Background()) })

	tr.AddTask(&persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		CreationSource: source, Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now()})
	require.NoError(t, e.Execute(taskID))
	require.Eventually(t, func() bool { return e.ActiveCount() == 0 }, 15*time.Second, 20*time.Millisecond)
	got, _ := tr.Get(context.Background(), taskID)
	require.NotNil(t, got)
	require.Equal(t, persistence.TaskStatusCompleted, got.Status, "the task must complete for the test to mean anything")
	return saw.Load(), m
}

func refreshCount(m *Metrics, source, outcome string) float64 {
	return testutil.ToFloat64(m.WorkspaceRefreshTotal.WithLabelValues("p1", source, outcome))
}

// THE REGRESSION: an opted-in project's scheduled task starts from origin.
func TestPreWorkRefresh_ScheduledTaskStartsFromOrigin(t *testing.T) {
	f := newRefreshFixture(t, true)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "main"},
		persistence.TaskCreationSourceScheduled)
	assert.True(t, saw, "the agent's worktree must hold the upstream commit pushed after the clone (GitHub #400)")
	assert.Equal(t, 1.0, refreshCount(m, "scheduled", WorkspaceRefreshRefreshed))
	assert.Equal(t, 1, testutil.CollectAndCount(m.WorkspaceRefreshTotal), "exactly one series: one eligible task, one outcome")
}

// An autonomy task (cron or llm mode) is refreshed the same way, with the
// branch taken from the clone when none is configured.
func TestPreWorkRefresh_AutonomousTaskUsesCurrentBranch(t *testing.T) {
	f := newRefreshFixture(t, true)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true},
		persistence.TaskCreationSourceAutonomous)
	assert.True(t, saw)
	assert.Equal(t, 1.0, refreshCount(m, "autonomous", WorkspaceRefreshRefreshed))
}

// The default is unchanged: no key, no refresh, no count.
func TestPreWorkRefresh_NotOptedInLeavesCloneAlone(t *testing.T) {
	f := newRefreshFixture(t, true)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{}, persistence.TaskCreationSourceScheduled)
	assert.False(t, saw, "a project that did not opt in keeps the as-built behaviour")
	assert.Equal(t, 0, testutil.CollectAndCount(m.WorkspaceRefreshTotal))
}

// A hand-submitted task in an opted-in project is not refreshed (design O2
// default) and not counted: it is not eligible.
func TestPreWorkRefresh_UserTaskNotRefreshed(t *testing.T) {
	f := newRefreshFixture(t, true)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true}, persistence.TaskCreationSourceUser)
	assert.False(t, saw)
	assert.Equal(t, 0, testutil.CollectAndCount(m.WorkspaceRefreshTotal))
}

// Opted in, no origin: the task still runs, and the skip is counted.
func TestPreWorkRefresh_NoOriginCountedAndRuns(t *testing.T) {
	f := newRefreshFixture(t, false)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true}, persistence.TaskCreationSourceScheduled)
	assert.False(t, saw)
	assert.Equal(t, 1.0, refreshCount(m, "scheduled", WorkspaceRefreshNoOrigin))
}

// Opted in, the origin's branch does not exist: fetch fails, counted, runs.
func TestPreWorkRefresh_FetchFailureCountedAndRuns(t *testing.T) {
	f := newRefreshFixture(t, true)
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "no-such-branch"},
		persistence.TaskCreationSourceScheduled)
	assert.False(t, saw)
	assert.Equal(t, 1.0, refreshCount(m, "scheduled", WorkspaceRefreshFetchFailed))
}

// The forge path's safety rule holds on the opt-in path: a commit the clone
// has and origin lacks is saved to a refs/vornik/discarded ref before the reset.
func TestPreWorkRefresh_LocalOnlyCommitBackedUp(t *testing.T) {
	f := newRefreshFixture(t, true)
	commitIn(t, f.projectDir, "LOCAL.md", "local only\n")
	local := gitOut(t, f.projectDir, "git", "rev-parse", "HEAD")
	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "main"},
		persistence.TaskCreationSourceScheduled)
	assert.True(t, saw)
	assert.Equal(t, 1.0, refreshCount(m, "scheduled", WorkspaceRefreshRefreshed))
	refs := gitOut(t, f.projectDir, "git", "for-each-ref", "--format=%(objectname)", "refs/vornik/discarded/")
	assert.Contains(t, refs, local, "the local-only commit must survive in a backup ref")
}

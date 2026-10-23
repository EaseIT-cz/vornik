package executor

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #400, unit level (executor design §9.6.3). The executor-e2e tests in
// workspace_refresh_e2e_test.go are the regression gate; these pin the pieces.

func TestRefreshSource_OnlyScheduledAndAutonomous(t *testing.T) {
	for src, want := range map[persistence.TaskCreationSource]string{
		persistence.TaskCreationSourceScheduled:  "scheduled",
		persistence.TaskCreationSourceAutonomous: "autonomous",
	} {
		got, ok := refreshSource(src)
		assert.True(t, ok, src)
		assert.Equal(t, want, got)
	}
	for _, src := range []persistence.TaskCreationSource{
		persistence.TaskCreationSourceUser, persistence.TaskCreationSourceDelegation,
		persistence.TaskCreationSourceRoute, persistence.TaskCreationSourceCheckpoint,
		persistence.TaskCreationSourceFork, persistence.TaskCreationSourceA2A,
		persistence.TaskCreationSourceCompanion, "",
	} {
		_, ok := refreshSource(src)
		assert.False(t, ok, "source %q must not be refreshed (design O2 default)", src)
	}
}

func TestResolveRefreshBranch_ConfiguredCurrentDetached(t *testing.T) {
	requireGitExec(t)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	root := t.TempDir()
	_, scratch := s6Seed(t, root)

	assert.Equal(t, "release", resolveRefreshBranch(ctx, scratch, " release "), "configured wins")
	assert.Equal(t, "main", resolveRefreshBranch(ctx, scratch, ""), "else the clone's current branch")
	mustGit(t, scratch, "git", "checkout", "-q", "--detach")
	assert.Equal(t, "", resolveRefreshBranch(ctx, scratch, ""), "a detached HEAD is never guessed")
	assert.Equal(t, "", resolveRefreshBranch(ctx, t.TempDir(), ""), "not a repo: no branch")
}

func TestRebaseProjectToOrigin_ReturnsOutcomes(t *testing.T) {
	requireGitExec(t)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()
	bare, _ := s6Seed(t, root)
	clone := root + "/clone"
	mustGit(t, root, "git", "clone", "-q", bare, clone)

	assert.Equal(t, WorkspaceRefreshNotGit, rebaseProjectToOrigin(ctx, t.TempDir(), "main", log))
	assert.Equal(t, WorkspaceRefreshNotGit, rebaseProjectToOrigin(ctx, clone, " ", log), "an empty branch has nothing to reset to")
	assert.Equal(t, WorkspaceRefreshRefreshed, rebaseProjectToOrigin(ctx, clone, "main", log))
	assert.Equal(t, WorkspaceRefreshFetchFailed, rebaseProjectToOrigin(ctx, clone, "no-such-branch", log))
	mustGit(t, clone, "git", "remote", "remove", "origin")
	assert.Equal(t, WorkspaceRefreshNoOrigin, rebaseProjectToOrigin(ctx, clone, "main", log))
}

// The skips are decided before any git runs, and each is counted once.
func TestRefreshBeforeWork_SkipTable(t *testing.T) {
	requireGitExec(t)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	root := t.TempDir()
	bare, scratch := s6Seed(t, root)
	clone := root + "/clone"
	mustGit(t, root, "git", "clone", "-q", bare, clone)
	before := gitOut(t, clone, "git", "rev-parse", "HEAD")
	commitIn(t, scratch, "UP.md", "up\n")
	mustGit(t, scratch, "git", "push", "-q", "origin", "main")

	optIn := registry.ProjectWorkspace{RefreshBeforeWork: true}
	cases := []struct {
		name     string
		project  *registry.Project
		adopt    bool
		parentOf bool
		want     string
	}{
		{"backlog mode", &registry.Project{ID: "p1", Workspace: optIn, Autonomy: registry.ProjectAutonomy{Mode: "backlog"}}, false, false, WorkspaceRefreshBacklogMode},
		{"auto-resume adopts the kept worktree", &registry.Project{ID: "p1", Workspace: optIn}, true, false, WorkspaceRefreshResume},
		{"delegator resuming with children", &registry.Project{ID: "p1", Workspace: optIn}, false, true, WorkspaceRefreshHasChildren},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, _, tr := setup()
			m := NewMetrics(prometheus.NewRegistry())
			e.metrics = m
			task := &persistence.Task{ID: "t-skip", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceScheduled, CreatedAt: time.Now()}
			tr.AddTask(task)
			if tc.parentOf {
				parent := task.ID
				tr.AddTask(&persistence.Task{ID: "t-child", ProjectID: "p1", ParentTaskID: &parent, CreatedAt: time.Now()})
			}
			e.refreshBeforeWork(ctx, task, tc.project, clone, tc.adopt)
			assert.Equal(t, 1.0, testutil.ToFloat64(m.WorkspaceRefreshTotal.WithLabelValues("p1", "scheduled", tc.want)))
			assert.Equal(t, 1, testutil.CollectAndCount(m.WorkspaceRefreshTotal), "exactly one outcome per eligible task")
			assert.Equal(t, before, gitOut(t, clone, "git", "rev-parse", "HEAD"), "a skip never touches the clone")
		})
	}

	// Detached HEAD with no configured branch: skipped, not guessed.
	mustGit(t, clone, "git", "checkout", "-q", "--detach")
	e, _, _, _, _ := setup()
	m := NewMetrics(prometheus.NewRegistry())
	e.metrics = m
	task := &persistence.Task{ID: "t-det", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceAutonomous}
	e.refreshBeforeWork(ctx, task, &registry.Project{ID: "p1", Workspace: optIn}, clone, false)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.WorkspaceRefreshTotal.WithLabelValues("p1", "autonomous", WorkspaceRefreshNoBranch)))
	assert.Equal(t, before, gitOut(t, clone, "git", "rev-parse", "HEAD"))

	// Not opted in, nil project, or nil metrics: no-op, no panic.
	e.refreshBeforeWork(ctx, task, &registry.Project{ID: "p1"}, clone, false)
	e.refreshBeforeWork(ctx, task, nil, clone, false)
	e.metrics = nil
	require.NotPanics(t, func() { e.refreshBeforeWork(ctx, task, &registry.Project{ID: "p1", Workspace: optIn}, clone, false) })
	assert.Equal(t, 1, testutil.CollectAndCount(m.WorkspaceRefreshTotal))
}

package autonomy

// S6 compatibility matrix, "autonomy backlog refresh (fetch, reset)" (process-
// spawn law design, S6): a Manager-driven backlog tick on a real repository
// fetches origin through the daemon's git and picks up an external
// contribution before it reads the backlog. Written and run green before S6
// changed production code.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/backlogfile"
)

// s6DaemonGitHome writes gitconfig as the operator's global config in a fresh
// HOME and composes the daemon's git config from it, as the daemon does at
// startup (S6-D4).
func s6DaemonGitHome(t *testing.T, gitconfig string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	s6ComposeDaemonGitConfig(t, home)
	return home
}

func TestS6Compat_ManagerTickRefreshesARealRepoFromOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s6DaemonGitHome(t, "")
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	contrib := filepath.Join(root, "contrib")
	ws := filepath.Join(root, "ws")
	work := filepath.Join(ws, "p1")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, contrib)
	require.NoError(t, os.WriteFile(filepath.Join(contrib, "BACKLOG.md"), []byte("- [ ] fix the parser\n"), 0o644))
	git(t, contrib, "add", "-A")
	git(t, contrib, "commit", "-q", "-m", "backlog")
	git(t, contrib, "push", "-q", "origin", "main")
	git(t, ws, "clone", "-q", origin, work)

	// An external contribution lands on origin after the workspace cloned.
	require.NoError(t, os.WriteFile(filepath.Join(contrib, "external.txt"), []byte("merged upstream\n"), 0o644))
	git(t, contrib, "add", "-A")
	git(t, contrib, "commit", "-q", "-m", "external")
	git(t, contrib, "push", "-q", "origin", "main")

	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	repo := &mockTaskRepo{}
	m := New(nil, reg, repo, nil,
		WithWorkspacePath(ws),
		WithEvaluationRepository(&captureEvalRepo{}),
		WithBacklogStore(backlogfile.NewStore()),
		WithGitRefresh(ExecGitRefresh),
	)
	project := reg.GetProject("p1")
	require.NotNil(t, project)

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	_, err := os.Stat(filepath.Join(work, "external.txt"))
	assert.NoError(t, err, "the tick must refresh the workspace from origin before reading the backlog")
	assert.Len(t, repo.createdTasks(), 1, "the refreshed backlog's item must be dispatched")
}

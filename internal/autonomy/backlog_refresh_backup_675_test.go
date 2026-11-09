package autonomy

// GitHub #675 (the rest of #484, operator decision A10): the backlog-mode
// tick's reset to origin keeps the clone's local-only commits and tracked
// uncommitted changes in refs/vornik/discarded/* refs, as the executor's
// task-start resets do; the backlog file's own [x]/[!] marks are not a reason
// to write a ref (executor design 06-executor.md §9.6.7).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/backlogfile"
)

func gitOut675(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func discardedRefs675(t *testing.T, dir string) []string {
	t.Helper()
	out := gitOut675(t, dir, "for-each-ref", "--format=%(refname)", "refs/vornik/discarded/")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// seedBacklogClone675 makes origin with a tracked BACKLOG.md and base.txt,
// and the project clone <ws>/p1. It returns the workspace root and the clone.
func seedBacklogClone675(t *testing.T) (string, string) {
	t.Helper()
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
	require.NoError(t, os.WriteFile(filepath.Join(contrib, "BACKLOG.md"), []byte("- [ ] fix the parser\n- [ ] add a test\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(contrib, "base.txt"), []byte("base\n"), 0o644))
	git(t, contrib, "add", "-A")
	git(t, contrib, "commit", "-q", "-m", "seed")
	git(t, contrib, "push", "-q", "origin", "main")
	git(t, ws, "clone", "-q", origin, work)
	return ws, work
}

func backlogManager675(t *testing.T, ws string) (*mockTaskRepo, func() error) {
	t.Helper()
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
	return repo, func() error { return m.tickBacklog(context.Background(), project, time.Now()) }
}

func TestBacklogTick_BacksUpDirtyTreeAndLocalCommits_675(t *testing.T) {
	ws, work := seedBacklogClone675(t)
	// A local-only commit (an unpushed merge-back) and, on top of it, an
	// unstaged change to a tracked file.
	require.NoError(t, os.WriteFile(filepath.Join(work, "local.txt"), []byte("local only\n"), 0o644))
	git(t, work, "add", "local.txt")
	git(t, work, "commit", "-q", "-m", "local only")
	localSHA := gitOut675(t, work, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(work, "base.txt"), []byte("dirty, unstaged\n"), 0o644))

	repo, tick := backlogManager675(t, ws)
	require.NoError(t, tick())

	var commitRef, dirtyRef string
	for _, r := range discardedRefs675(t, work) {
		if strings.Contains(r, "-dirty-") {
			dirtyRef = r
		} else {
			commitRef = r
		}
	}
	require.NotEmpty(t, commitRef, "the tick's reset must keep the local-only commit in a refs/vornik/discarded/* ref")
	assert.Equal(t, localSHA, gitOut675(t, work, "rev-parse", commitRef))
	require.NotEmpty(t, dirtyRef, "the tick's reset must keep the tracked uncommitted change in a *-dirty-* ref")
	assert.Equal(t, "dirty, unstaged", gitOut675(t, work, "show", dirtyRef+":base.txt"))

	assert.Equal(t, gitOut675(t, work, "rev-parse", "origin/main"), gitOut675(t, work, "rev-parse", "HEAD"), "the clone is still reset to origin")
	body, err := os.ReadFile(filepath.Join(work, "base.txt"))
	require.NoError(t, err)
	assert.Equal(t, "base\n", string(body))
	assert.Len(t, repo.createdTasks(), 1, "the backlog item is still dispatched")
}

func TestBacklogTick_MarksAloneWriteNoBackup_675(t *testing.T) {
	ws, work := seedBacklogClone675(t)
	repo, tick := backlogManager675(t, ws)
	// The first tick dispatches an item and marks it in BACKLOG.md: an
	// uncommitted change to a tracked file that the store itself made.
	require.NoError(t, tick())
	require.Len(t, repo.createdTasks(), 1)
	require.NotEmpty(t, gitOut675(t, work, "status", "--porcelain", "-uno"), "precondition: the marks are a tracked change")

	// The next ticks reset over those marks and re-apply them.
	require.NoError(t, tick())
	require.NoError(t, tick())
	assert.Empty(t, discardedRefs675(t, work), "the backlog file's own marks must not write a backup ref on every tick")
}

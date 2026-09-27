package executor

// S6 compatibility matrix, executor half (process-spawn law design, "S6 — git
// state an agent can write", compatibility matrix). Git is the product's most
// critical capability: forge change requests, issue-fix, merge-back, workspace
// versioning and rollback all depend on it. Every test here pins a flow that
// WORKS, against real git, and was written and run green BEFORE S6 changed a
// line of production code — so a regression shows as a failing test here, not
// as a lost change request.

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// s6DaemonGitHome stands in for the operator's environment and the daemon's
// startup: it writes gitconfig as the operator's GLOBAL config in a fresh
// HOME, points HOME and XDG_CONFIG_HOME at it, and composes the daemon's git
// config from it the way the daemon does at startup (S6-D4).
func s6DaemonGitHome(t *testing.T, gitconfig string) {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	s6ComposeDaemonGitConfig(t, home)
}

// s6Script writes an executable sh script and returns its path.
func s6Script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755))
	return p
}

// s6Seed makes a bare "remote.git" with one commit on main, plus a scratch
// clone of it used to author upstream commits. Returns (bare, scratch).
func s6Seed(t *testing.T, root string) (string, string) {
	t.Helper()
	bare := filepath.Join(root, "remote.git")
	scratch := filepath.Join(root, "scratch")
	mustGit(t, root, "git", "init", "-q", "--bare", "-b", "main", bare)
	mustGit(t, root, "git", "clone", "-q", bare, scratch)
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "base.txt"), []byte("base\n"), 0o644))
	mustGit(t, scratch, "git", "add", "-A")
	mustGit(t, scratch, "git", "commit", "-q", "-m", "base")
	mustGit(t, scratch, "git", "push", "-q", "origin", "main")
	return bare, scratch
}

// Forge PR-review checkout (fetch head, reset) against a remote that DEMANDS
// credentials: the operator's credential helper must still be consulted, or
// every review of a private repository breaks. The helper is a `!` shell form,
// the shape `gh auth git-credential` is installed as.
func TestS6Compat_ForgeReviewCheckoutConsultsCredentialHelper(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare, scratch := s6Seed(t, root)
	mustGit(t, scratch, "git", "checkout", "-q", "-b", "pr")
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "pr.txt"), []byte("from the PR\n"), 0o644))
	mustGit(t, scratch, "git", "add", "-A")
	mustGit(t, scratch, "git", "commit", "-q", "-m", "pr work")
	prSHA := gitOut(t, scratch, "git", "rev-parse", "HEAD")
	mustGit(t, scratch, "git", "push", "-q", "origin", "pr:refs/pull/7/head")

	// A smart-HTTP remote that answers 401 without the right Basic auth.
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer srv.Close()

	clone := filepath.Join(root, "project")
	mustGit(t, root, "git", "clone", "-q", bare, clone)
	mustGit(t, clone, "git", "remote", "set-url", "origin", srv.URL+"/remote.git")

	home := t.TempDir()
	marker := filepath.Join(home, "helper-called")
	helper := s6Script(t, home, "cred.sh",
		`echo "$1" >> "`+marker+`"
if [ "$1" = get ]; then echo username=u; echo password=p; fi
`)
	s6DaemonGitHome(t, "[credential]\n\thelper = !"+helper+"\n")

	checkoutForgeChangeRequest(context.Background(), clone, "refs/pull/7/head", "main", zerolog.Nop())

	assert.Equal(t, prSHA, gitOut(t, clone, "git", "rev-parse", "HEAD"),
		"the review checkout must land the change request's head")
	calls, err := os.ReadFile(marker)
	require.NoError(t, err, "the operator's credential helper was never consulted")
	assert.Contains(t, string(calls), "get")
}

// Forge fetch over SSH: the operator's core.sshCommand is what runs, and the
// pre-work rebase still reaches upstream through it (G3).
func TestS6Compat_ForgeFetchOverSSHUsesOperatorSSHCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare, scratch := s6Seed(t, root)
	clone := filepath.Join(root, "project")
	mustGit(t, root, "git", "clone", "-q", bare, clone)
	mustGit(t, clone, "git", "remote", "set-url", "origin", "fakehost:"+bare)

	require.NoError(t, os.WriteFile(filepath.Join(scratch, "up.txt"), []byte("upstream\n"), 0o644))
	mustGit(t, scratch, "git", "add", "-A")
	mustGit(t, scratch, "git", "commit", "-q", "-m", "upstream")
	mustGit(t, scratch, "git", "push", "-q", "origin", "main")
	upSHA := gitOut(t, scratch, "git", "rev-parse", "HEAD")

	home := t.TempDir()
	marker := filepath.Join(home, "ssh-called")
	ssh := s6FakeSSH(t, home, marker)
	s6DaemonGitHome(t, "[core]\n\tsshCommand = "+ssh+"\n[ssh]\n\tvariant = simple\n")

	rebaseProjectToOrigin(context.Background(), clone, "main", zerolog.Nop())

	assert.Equal(t, upSHA, gitOut(t, clone, "git", "rev-parse", "HEAD"),
		"the rebase must reach upstream over the operator's ssh command")
	calls, err := os.ReadFile(marker)
	require.NoError(t, err, "the operator's core.sshCommand never ran")
	assert.Contains(t, string(calls), "fakehost")
}

// s6FakeSSH is an ssh stand-in that runs the remote side locally: git (with
// ssh.variant=simple) calls it as `<cmd> <host> <remote command>`.
func s6FakeSSH(t *testing.T, dir, marker string) string {
	t.Helper()
	return s6Script(t, dir, "fakessh", `echo "$*" >> "`+marker+`"
shift
exec sh -c "$(echo "$*" | sed 's/^git-/git /')"
`)
}

// The operator's global ignores (core.excludesFile) keep a file out of the
// auto-commit and so out of the merge-back (G2).
func TestS6Compat_GlobalExcludesHonouredByAutoCommitAndMergeBack(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	home := t.TempDir()
	ignore := filepath.Join(home, "ignore")
	require.NoError(t, os.WriteFile(ignore, []byte("*.secret\n"), 0o644))
	s6DaemonGitHome(t, "[core]\n\texcludesFile = "+ignore+"\n")

	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_excludes", zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "kept.txt"), []byte("kept\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "token.secret"), []byte("s3cret\n"), 0o644))

	require.NoError(t, mergeWorktree(ctx, projectDir, wt, "task_s6_excludes", "", zerolog.Nop()))

	tree := gitOut(t, projectDir, "git", "ls-tree", "-r", "--name-only", "HEAD")
	assert.Contains(t, tree, "kept.txt")
	assert.NotContains(t, tree, "token.secret", "a globally ignored file must not be committed")
}

// A driver the project defines in its OWN .git/config (git-lfs style, `git lfs
// install --local`) is the operator's choice and DOES run on merge-back (G5).
func TestS6Compat_ProjectLocalDriverRunsOnMergeBack(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	home := t.TempDir()
	marker := filepath.Join(home, "project-filter-ran")
	clean := s6Script(t, home, "clean.sh", `echo ran >> "`+marker+`"
cat
`)
	mustGit(t, projectDir, "git", "config", "filter.projlocal.clean", clean)

	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_localdriver", zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("*.dat filter=projlocal\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "x.dat"), []byte("payload\n"), 0o644))

	require.NoError(t, mergeWorktree(ctx, projectDir, wt, "task_s6_localdriver", "", zerolog.Nop()))

	_, err = os.Stat(marker)
	assert.NoError(t, err, "a filter defined in the project's own .git/config must run")
	assert.Equal(t, "payload\n", gitOut(t, projectDir, "git", "show", "HEAD:x.dat")+"\n")
}

// HEAD sampling, format-patch and the role-claim git reads on a real worktree
// return the worktree's true values.
func TestS6Compat_HeadPatchesAndClaimsOnARealWorktree(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_head", zerolog.Nop())
	require.NoError(t, err)
	from := gitHEAD(ctx, wt)
	require.NotEmpty(t, from)

	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	mustGit(t, wt, "git", "add", "-A")
	mustGit(t, wt, "git", "commit", "-q", "-m", "agent work")
	want := gitOut(t, wt, "git", "rev-parse", "HEAD")

	assert.Equal(t, want, gitHEAD(ctx, wt))
	n, ok := gitDiffFileCount(ctx, wt, from, want)
	assert.True(t, ok)
	assert.Equal(t, 1, n)
	assert.True(t, gitObjectExists(ctx, wt, want))

	sum, err := generatePlanChanges(ctx, wt, from, want)
	require.NoError(t, err)
	require.NotNil(t, sum)
	defer func() { _ = os.RemoveAll(sum.OutputDir) }()
	assert.Len(t, sum.Patches, 1)
	assert.Contains(t, sum.Summary, "agent work")
}

// Worktree create then remove leaves no directory, no admin dir, no branch.
func TestS6Compat_WorktreeCreateRemoveLeavesNothing(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	s6DaemonGitHome(t, "")
	ctx := context.Background()
	wt, err := createWorktree(ctx, projectDir, "task_s6_cr", zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, worktreePath(projectDir, "task_s6_cr"), wt)

	removeWorktree(ctx, projectDir, wt, "task_s6_cr", zerolog.Nop())

	_, err = os.Stat(wt)
	assert.True(t, os.IsNotExist(err), "worktree directory must be gone")
	_, err = os.Stat(filepath.Join(projectDir, ".git", "worktrees", "task_s6_cr"))
	assert.True(t, os.IsNotExist(err), "admin dir must be gone")
	assert.Empty(t, gitOut(t, projectDir, "git", "branch", "--list", worktreeBranch("task_s6_cr")))
}

// s6OneStepResolver is a single agent-step workflow for project p1.
func s6OneStepResolver() *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "localhost/vornik-agent:test"}}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "run",
			Steps:     map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	}
}

// The cancel path end to end on real git: a running task's worktree (with the
// agent's uncommitted work) is removed, its branch deleted, and the project's
// HEAD and tree are untouched.
func TestS6Compat_CancelPathOnRealGit(t *testing.T) {
	s6DaemonGitHome(t, "")
	root := newGitWorkspaceForProject(t, "p1")
	projectDir := filepath.Join(root, "p1")
	before := gitOut(t, projectDir, "git", "rev-parse", "HEAD")

	e, rt, _, _, tr := setup()
	e.config.ProjectWorkspacePath = root
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(s6OneStepResolver())
	rt.outputJSON = `{"status":"COMPLETED"}`
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt.mu.Lock()
	rt.waitGate, rt.waitEntered = gate, entered
	rt.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		_ = e.Stop(context.Background())
	})

	tr.AddTask(&persistence.Task{ID: "t-s6-cancel", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t-s6-cancel"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never reached its container")
	}
	rt.mu.Lock()
	wt := rt.lastConfig.ProjectDir
	rt.mu.Unlock()
	require.Equal(t, worktreePath(projectDir, "t-s6-cancel"), wt, "the agent must get the task's worktree")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "half-done.txt"), []byte("wip\n"), 0o644))

	require.NoError(t, e.Cancel("t-s6-cancel"))

	assert.Eventually(t, func() bool {
		_, err := os.Stat(wt)
		return os.IsNotExist(err) && e.ActiveCount() == 0
	}, 10*time.Second, 20*time.Millisecond, "the cancelled task's worktree must be removed")
	assert.Equal(t, before, gitOut(t, projectDir, "git", "rev-parse", "HEAD"), "cancel must not move the project")
	assert.Empty(t, gitOut(t, projectDir, "git", "branch", "--list", "worktree/*"))
	_, err := os.Stat(filepath.Join(projectDir, "half-done.txt"))
	assert.True(t, os.IsNotExist(err), "the cancelled task's work must not reach the project")
	assert.Empty(t, strings.TrimSpace(gitOut(t, projectDir, "git", "status", "--porcelain")))
}

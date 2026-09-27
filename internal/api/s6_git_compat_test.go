package api

// S6 compatibility matrix, "git over HTTPS (clone/push, receive guards)"
// (process-spawn law design, S6, G1): an operator whose GLOBAL git config sets
// core.hooksPath must not redirect `git http-backend` away from the project's
// own .git/hooks, where the daemon's receive guard lives. A push to a reserved
// worktree/* branch is still refused. Written and run green before S6 changed
// production code.

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// s6DaemonGitHome writes gitconfig as the operator's global config in a fresh
// HOME and composes the daemon's git config from it, as the daemon does at
// startup (S6-D4).
func s6DaemonGitHome(t *testing.T, gitconfig string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	s6ComposeDaemonGitConfig(t, home)
	return home
}

func TestS6Compat_ReceiveGuardHoldsWithAGlobalHooksPath(t *testing.T) {
	requireGit(t)
	emptyHooks := t.TempDir()
	s6DaemonGitHome(t, "[core]\n\thooksPath = "+emptyHooks+"\n")

	root := t.TempDir()
	proj := "proj_s6_hookspath"
	repo := newPushRepo(t, root, proj)

	srv, _ := pushRouter(t, root, emptyCounts(), realGuardsEnsurer(root))
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	mustGit(t, clone, "checkout", "-q", "-b", "worktree/evil")
	gitWriteFile(t, filepath.Join(clone, "x.txt"), "x\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "evil")

	out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", "worktree/evil").CombinedOutput()
	if err == nil {
		t.Fatalf("a push to refs/heads/worktree/* must be refused even with a global core.hooksPath\n%s", out)
	}
	if ref, _ := exec.Command("git", "-C", repo, "rev-parse", "-q", "--verify", "refs/heads/worktree/evil").Output(); len(ref) != 0 {
		t.Fatalf("the reserved ref reached the project: %s", ref)
	}
}

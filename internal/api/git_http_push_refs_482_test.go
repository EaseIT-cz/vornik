package api

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #482 (design 22 "Amendment 2026-10-21 (#482)";
// git-workspace-https-access design §4.3 item 3, amended): the push guard is
// an allowlist. A key with allow_push could otherwise set the clone's
// refs/remotes/origin/main, or a replace ref substituting origin/main's
// commit, to a forged design, and the memory provenance verifier reads that
// ref as "merged to main". Found at: review (design round 1, review fa0c F1).
// Regression test at: the git-over-HTTPS push handler, the level a pushing
// key reaches.
func TestGitPush_HookRejectsRemoteTrackingAndReplaceRefs(t *testing.T) {
	root := t.TempDir()
	proj := "proj_refs"
	repo := newPushRepo(t, root, proj)
	srv, auditRepo := pushRouter(t, root,
		&fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}},
		realGuardsEnsurer(root))
	ts := httptest.NewServer(gitRegisteredRouter(srv, auditRepo))
	defer ts.Close()

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	realOut, err := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	realHead := strings.TrimSpace(string(realOut))
	gitWriteFile(t, filepath.Join(clone, "docs", "low-level-design", "22.md"), "forged ruling\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "forged")

	for _, refspec := range []string{
		"HEAD:refs/remotes/origin/main",
		"HEAD:refs/replace/" + realHead,
		"HEAD:refs/notes/commits",
		// A branch named exactly "worktree" would block every
		// refs/heads/worktree/<task> the daemon creates (code review 0780 F6).
		"HEAD:refs/heads/worktree",
		// Round-3 review B8: deleting the default branch is refused by name
		// (it was refused only because merge-base fails on the zero id).
		":refs/heads/main",
	} {
		if out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", refspec).CombinedOutput(); err == nil {
			t.Errorf("push %s was accepted; want the pre-receive allowlist to refuse it\n%s", refspec, out)
		}
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main").CombinedOutput(); err == nil {
		t.Fatalf("refs/remotes/origin/main exists in the workspace after the refused push: %s", out)
	}
	if out, _ := exec.Command("git", "-C", repo, "rev-parse", "--glob=refs/replace/*").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a replace ref exists in the workspace after the refused push: %s", out)
	}
	// The allowlist keeps branches and tags pushable.
	for _, refspec := range []string{"HEAD:refs/heads/feature", "HEAD:refs/tags/v1"} {
		if out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", refspec).CombinedOutput(); err != nil {
			t.Errorf("push %s was refused: %v\n%s", refspec, err, out)
		}
	}
}

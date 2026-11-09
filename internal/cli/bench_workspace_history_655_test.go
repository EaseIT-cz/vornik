package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

// Each repeat starts on a one-commit history (benchmark LLD §12.30, GitHub
// #655).
//
// Incident, found at production (operator observation of the 2026-11-06
// recalibration): lh-06 r2 recovered its predecessor's spec with
// `git show 0082b89:.autonomy/CURRENT_TASK.md`. §12.29 removed the state files
// with a commit, so every earlier task's work stayed in the history the agent
// container mounts read-only.
//
// Lower because: the reset is gitWorkspace.Prepare, run here on a real git
// repository exactly as vornikctl runs it before each bench task; the bench
// run that follows the fix is the e2e check.

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitObjects counts commit objects in the whole object store.
func commitObjects(t *testing.T, dir string) int {
	t.Helper()
	out := gitOut(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if l == "commit" {
			n++
		}
	}
	return n
}

func historyWorkspace(t *testing.T) (dir, oldHead string) {
	t.Helper()
	dir = gitRepo(t)
	commitFile(t, dir, ".autonomy/CURRENT_TASK.md", "# CURRENT TASK — scratch/lhworker\n")
	commitFile(t, dir, "scratch/lhworker/backoff.go", "package lhworker\n")
	commitFile(t, dir, "scratch/other/other.go", "package other\n")
	gitOut(t, dir, "tag", "v-old")
	if err := os.WriteFile(filepath.Join(dir, "scratch/other/other.go"), []byte("package other // stashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "stash", "-q")
	oldHead = gitOut(t, dir, "rev-parse", "HEAD")
	// ORIG_HEAD naming an old commit must not keep it alive.
	if err := os.WriteFile(filepath.Join(dir, ".git", "ORIG_HEAD"), []byte(oldHead+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, oldHead
}

func TestGitWorkspace_PrepareLeavesOneCommitAndArchivesTheRest_655(t *testing.T) {
	dir, oldHead := historyWorkspace(t)
	treeBefore := gitOut(t, dir, "rev-parse", "HEAD^{tree}")
	ws := gitWorkspace{dir: dir}
	spec := agentbench.TaskSpec{ID: "lh-06", Targets: []string{"scratch/lhpipe"}}

	commit, err := ws.Prepare(context.Background(), spec, 2)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if n := commitObjects(t, dir); n != 1 {
		t.Fatalf("the workspace holds %d commit objects after the reset, want 1", n)
	}
	if all := gitOut(t, dir, "rev-list", "--all"); all != commit {
		t.Fatalf("rev-list --all = %q, want only the returned root %q", all, commit)
	}
	if refs := gitOut(t, dir, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/master" {
		t.Fatalf("refs after the reset = %q, want only refs/heads/master", refs)
	}
	if err := exec.Command("git", "-C", dir, "cat-file", "-e", oldHead).Run(); err == nil {
		t.Fatal("the pre-reset HEAD is still readable from the workspace")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "ORIG_HEAD")); !os.IsNotExist(err) {
		t.Fatal("ORIG_HEAD must be removed")
	}
	msg := gitOut(t, dir, "log", "-1", "--format=%s")
	if !strings.Contains(msg, "lh-06 r2") || !strings.Contains(msg, "§12.29") || !strings.Contains(msg, "§12.30") {
		t.Fatalf("the root commit names the task, the repeat and the rule: %q", msg)
	}
	// Only the state went; the rest of the tree is as the clear left it.
	if _, err := os.Stat(filepath.Join(dir, "scratch/other/other.go")); err != nil {
		t.Fatal("untargeted files stay")
	}
	if gitOut(t, dir, "rev-parse", "HEAD^{tree}") == treeBefore {
		t.Fatal("the tree must differ from before by exactly the cleared state")
	}
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("the reset leaves a clean workspace: %q", status)
	}

	// Everything is in the archive, outside the workspace.
	archive := dir + ".history.git"
	if strings.HasPrefix(archive, dir+string(filepath.Separator)) {
		t.Fatal("the archive must be outside the workspace directory")
	}
	if err := exec.Command("git", "--git-dir", archive, "cat-file", "-e", oldHead).Run(); err != nil {
		t.Fatalf("the archive must hold the pre-reset HEAD: %v", err)
	}
	refs := gitOut(t, "", "--git-dir", archive, "for-each-ref", "--format=%(refname)")
	for _, want := range []string{"/refs/heads/master", "/refs/tags/v-old", "/refs/stash"} {
		found := false
		for _, r := range strings.Split(refs, "\n") {
			if strings.HasPrefix(r, "refs/bench/lh-06/r2/") && strings.HasSuffix(r, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("archive refs %q lack refs/bench/lh-06/r2/<time>%s", refs, want)
		}
	}

	// A second reset of the same task and repeat (a retry after a failure
	// part-way) archives beside the first, never over it.
	commitFile(t, dir, ".autonomy/CURRENT_TASK.md", "retry\n")
	if _, err := ws.Prepare(context.Background(), spec, 2); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := exec.Command("git", "--git-dir", archive, "cat-file", "-e", oldHead).Run(); err != nil {
		t.Fatal("a retried reset must not lose the history archived by the first")
	}
	masters := 0
	for _, r := range strings.Split(gitOut(t, "", "--git-dir", archive, "for-each-ref", "--format=%(refname)"), "\n") {
		if strings.HasPrefix(r, "refs/bench/lh-06/r2/") && strings.HasSuffix(r, "/refs/heads/master") {
			masters++
		}
	}
	if masters != 2 {
		t.Fatalf("two attempts at lh-06 r2 archive two masters, got %d", masters)
	}

	// Nothing left to reset: a no-op.
	again, err := ws.Prepare(context.Background(), spec, 3)
	if err != nil || again != "" {
		t.Fatalf("a one-commit history with nothing to clear is a no-op: %q %v", again, err)
	}
}

func TestGitWorkspace_PrepareRefusesALiveWorktree_655(t *testing.T) {
	dir, oldHead := historyWorkspace(t)
	// The daemon's worktrees live under <project>/.worktrees, which git ignores.
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, ".worktrees", "task_live")
	gitOut(t, dir, "worktree", "add", "-q", wt, "-b", "task_live")
	_, err := gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1)
	if err == nil || !strings.Contains(err.Error(), wt) {
		t.Fatalf("a live worktree must refuse the reset and be named: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "cat-file", "-e", oldHead).Run(); err != nil {
		t.Fatal("a refused reset leaves the history untouched")
	}
}

func TestGitWorkspace_PreparePrunesAStaleWorktreeRecord_655(t *testing.T) {
	dir, _ := historyWorkspace(t)
	wt := filepath.Join(t.TempDir(), "crashed")
	gitOut(t, dir, "worktree", "add", "-q", wt, "-b", "task_crashed")
	if err := os.RemoveAll(wt); err != nil { // the daemon died, its directory went
		t.Fatal(err)
	}
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1); err != nil {
		t.Fatalf("a worktree record whose directory is gone is pruned, not refused: %v", err)
	}
	if n := commitObjects(t, dir); n != 1 {
		t.Fatalf("%d commit objects, want 1", n)
	}
}

func TestGitWorkspace_PrepareRefusesAlternates_655(t *testing.T) {
	dir, _ := historyWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "info", "alternates"), []byte("/elsewhere/objects\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1); err == nil || !strings.Contains(err.Error(), "alternates") {
		t.Fatalf("an alternates file must refuse the reset: %v", err)
	}
}

// A reset that failed part-way (here: the branch already moved to a root,
// a tag and the old objects still there) converges on the next Prepare.
func TestGitWorkspace_PrepareRecoversAHalfDoneReset_655(t *testing.T) {
	dir, oldHead := historyWorkspace(t)
	tree := gitOut(t, dir, "rev-parse", "HEAD^{tree}")
	root := gitOut(t, dir, "commit-tree", "--no-gpg-sign", tree, "-m", "half-done")
	gitOut(t, dir, "update-ref", "refs/heads/master", root)
	ws := gitWorkspace{dir: dir}
	if _, err := ws.Prepare(context.Background(), agentbench.TaskSpec{ID: "lh-06"}, 2); err != nil {
		t.Fatalf("a half-done reset must converge: %v", err)
	}
	if n := commitObjects(t, dir); n != 1 {
		t.Fatalf("%d commit objects, want 1", n)
	}
	if err := exec.Command("git", "-C", dir, "cat-file", "-e", oldHead).Run(); err == nil {
		t.Fatal("the old history is still in the workspace")
	}
	if err := exec.Command("git", "--git-dir", dir+".history.git", "cat-file", "-e", oldHead).Run(); err != nil {
		t.Fatal("the old history, still reachable from the tag, must be archived")
	}
}

// commit.gpgsign from the user's config must not fail the reset.
func TestGitWorkspace_PrepareIgnoresCommitSigning_655(t *testing.T) {
	dir, _ := historyWorkspace(t)
	gitOut(t, dir, "config", "commit.gpgsign", "true")
	gitOut(t, dir, "config", "gpg.program", "false") // any signing attempt fails
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1); err != nil {
		t.Fatalf("the root commit is unsigned by design: %v", err)
	}
}

// A pack kept with a .keep file survives gc; the verification refuses.
func TestGitWorkspace_PrepareRefusesWhenGCCannotPrune_655(t *testing.T) {
	dir, _ := historyWorkspace(t)
	gitOut(t, dir, "repack", "-a", "-d", "-q")
	packs, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		t.Fatal("no pack")
	}
	keep := strings.TrimSuffix(packs[0], ".pack") + ".keep"
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1)
	if err == nil || !strings.Contains(err.Error(), "commit objects after the reset") {
		t.Fatalf("an old commit that gc cannot remove must refuse the reset: %v", err)
	}
}

// GitHub #660: the harness runs under an env -i allowlist with no git
// identity, and the reset commit failed with "Author identity unknown",
// stopping the arm. Found at: e2e-lane (a bench run). Regression test at:
// integration, Prepare on a real repository with no identity anywhere.
// Lower because: the e2e lane is the operator's bench host.
func TestGitWorkspace_PrepareNeedsNoGitIdentity_660(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "none"))
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k) //nolint:errcheck // t.Setenv restores it
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		// Fixture commits name their author inline; the repository itself has
		// no identity, as on a bench host.
		full := append([]string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=f@example.com", "-c", "commit.gpgsign=false"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "master")
	for _, f := range []string{"BACKLOG.md", "scratch/a/a.go"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", f)
		run("commit", "-q", "-m", "add "+f)
	}
	commit, err := gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "dp-03-metric"}, 1)
	if err != nil || commit == "" {
		t.Fatalf("a reset with no git identity configured: %q %v", commit, err)
	}
	if who := gitOut(t, dir, "log", "-1", "--format=%an <%ae> / %cn <%ce>"); who != "vornik-bench <bench@vornik.io> / vornik-bench <bench@vornik.io>" {
		t.Fatalf("the reset commit's identity = %q", who)
	}
}

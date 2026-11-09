package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #659 (executor design §9.1, amended 2026-11-07). Found at: e2e-lane
// (a bench workspace made by hand with `git init` and no commit): the
// bootstrap was skipped because .git existed, and every task then failed at
// merge with "Non-fast-forward commit does not make sense into an empty head".
// Regression test at: integration, ensureGitRepo on real repositories.
// Lower because: the e2e lane needs a daemon and an agent run; the defect is
// the bootstrap decision, made here exactly as runExecution makes it.

func commitCount(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		return "no HEAD"
	}
	return strings.TrimSpace(string(out))
}

func TestEnsureGitRepo_659_EmptyInitGetsTheSeed(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "trunk").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := ensureGitRepo(context.Background(), dir, zerolog.Nop()); err != nil {
		t.Fatalf("ensureGitRepo: %v", err)
	}
	if n := commitCount(t, dir); n != "1" {
		t.Fatalf("an empty repository must get one bootstrap commit, got %s", n)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vornik", "bootstrap.md")); err != nil {
		t.Errorf("with no files, the marker is the seed: %v", err)
	}
	if b, _ := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output(); strings.TrimSpace(string(b)) != "trunk" {
		t.Errorf("the repository's own branch name must be kept, got %q", b)
	}
}

func TestEnsureGitRepo_659_EmptyInitWithFilesCommitsThem(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitRepo(context.Background(), dir, zerolog.Nop()); err != nil {
		t.Fatalf("ensureGitRepo: %v", err)
	}
	if n := commitCount(t, dir); n != "1" {
		t.Fatalf("want one bootstrap commit, got %s", n)
	}
	if files, _ := exec.Command("git", "-C", dir, "ls-files").Output(); !strings.Contains(string(files), "README.md") {
		t.Errorf("the repository's own files must be in the seed: %q", files)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vornik", "bootstrap.md")); !os.IsNotExist(err) {
		t.Error("with files present, no marker is needed")
	}
}

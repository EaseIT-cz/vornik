package agentloop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// audit 2026-10-12 (#142 #19, outcome-quality LLD §S6 #19): the agentloop git
// tools ran git through exec.Command with no context, so a git that never
// returned (an external diff driver, a credential or fsmonitor helper, a
// wedged filesystem) held the tool call until the container's own step
// timeout killed the whole step. Each git call is now bounded by
// gitCallTimeout, and its children are cut loose after gitWaitDelay.
//
// The hang is real, not simulated: the repository's diff.external points at a
// script that sleeps, and `git diff` honours diff.external by default.
func TestGitDiff_HungGitIsBounded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	repo := filepath.Join(ws, "project")
	script := filepath.Join(t.TempDir(), "slow-diff.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "config", "user.email", "t@example.invalid"},
		{"-C", repo, "config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", argv, out, err)
		}
	}
	file := filepath.Join(repo, "a.txt")
	if err := os.WriteFile(file, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"-C", repo, "add", "a.txt"},
		{"-C", repo, "commit", "-q", "-m", "init"},
		{"-C", repo, "config", "diff.external", script},
	} {
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", argv, out, err)
		}
	}
	if err := os.WriteFile(file, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevTimeout, prevDelay := gitCallTimeout, gitWaitDelay
	gitCallTimeout, gitWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { gitCallTimeout, gitWaitDelay = prevTimeout, prevDelay })

	raw, _ := json.Marshal(map[string]string{"path": "project"})
	done := make(chan string, 1)
	go func() { done <- Dispatch(Env{Workspace: ws}, "git_diff", raw) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "timed out") {
			t.Fatalf("a git diff cut off by its bound must say so, got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("git_diff did not return: the git call has no bound")
	}
}

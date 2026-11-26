//go:build !windows

package spawn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// GitHub #913 (process-spawn law design, Amendment 2026-11-25 (#913)): a git
// command killed at its deadline killed only git; a child it forked kept
// running. Here git fetch runs an ssh transport (GIT_SSH_COMMAND) that forks
// a long sleep. After the deadline, the sleep must be gone and Wait must
// return promptly.
func TestGitKind_DeadlineKillsTheWholeGroup_913(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	pidFile := filepath.Join(dir, "child.pid")
	fakeSSH := filepath.Join(dir, "fake-ssh.sh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	init := newGitCmd(context.Background(), KindGitWorkspace, []string{"init", "-q", repo})
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	c := newGitCmd(ctx, KindGitWorkspace, []string{"-C", repo, "fetch", "ssh://example.invalid/r.git"})
	c.c.Env = append(os.Environ(), "GIT_SSH_COMMAND="+fakeSSH, "GIT_TERMINAL_PROMPT=0")
	start := time.Now()
	_ = c.Run()
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Run returned after %v: the kill did not release Wait", took)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fake transport never started its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad child pid %q", raw)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("git's grandchild %d outlived the deadline kill (#913)", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

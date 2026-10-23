package chat

// GitHub #142 item 1 (2026-10-14, outcome-quality-design §S8 F1): a CLI
// provider call whose inner timeout fired must RETURN, as a timeout, even
// when the CLI left a descendant holding its stdout open. Before the fix the
// kill reached only the direct child; the stdout read in parseStreamJSON /
// parseCodexStream then blocked forever on the descendant's open pipe, so the
// call never returned, held its priority-queue worker for the life of the
// process, and wrote no outcome at all: the hung-not-failed shape #142 names.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"vornik.io/vornik/internal/spawn"
)

// writeOrphaningCLI writes a fake CLI that forks a long sleep sharing its
// stdout, records the sleep's pid, then waits. Killing the script leaves the
// sleep holding the pipe, which is the shape of a CLI whose helper process
// outlives it.
func writeOrphaningCLI(t *testing.T) (path string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs /bin/sh process semantics")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	path = filepath.Join(dir, "fake-cli")
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return path
}

func withShortCLIWaitDelay(t *testing.T) {
	t.Helper()
	old := cliWaitDelay
	cliWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { cliWaitDelay = old })
}

// assertReturnsAsTimeout runs call and fails the test when it has not
// returned well after the inner timeout plus the wait delay.
func assertReturnsAsTimeout(t *testing.T, name string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s: a call cut off by its timeout returned nil", name)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: err = %v, want it to wrap context.DeadlineExceeded", name, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the call has not returned 10s after its 300ms timeout: "+
			"a descendant holding stdout hangs the provider (GitHub #142)", name)
	}
}

func TestCLIClient_ADescendantHoldingStdoutDoesNotHangTheCall(t *testing.T) {
	withShortCLIWaitDelay(t)
	bin := writeOrphaningCLI(t)
	c := NewCLIClient("model-x",
		WithCLIBinary(spawn.NewConfiguredCommand(bin)),
		WithCLITimeout(300*time.Millisecond))
	assertReturnsAsTimeout(t, "claude cli", func() error {
		_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
		return err
	})
}

// GitHub #369 item 3 (2026-10-17, outcome-quality-design §S8.8, §S8.3 #3):
// found at review (the §S8 census; F1's cost column), observed at the provider
// call. F1 made the call return, but the helper the CLI forked lived on: the
// kill reached the CLI only, so every timed-out call leaked one process. The
// timeout now kills the CLI's process group. cliWaitDelay is set far above
// the assertion window, so a return inside it is the group kill's doing, not
// WaitDelay's.
func assertTimeoutKillsDescendant(t *testing.T, name string, pidFile string, call func() error) {
	t.Helper()
	old := cliWaitDelay
	cliWaitDelay = 30 * time.Second
	t.Cleanup(func() { cliWaitDelay = old })
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: err = %v, want it to wrap context.DeadlineExceeded", name, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%s: returned after %s: only WaitDelay returned it, the descendant was not killed", name, took)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: the call did not return within 20s of its 300ms timeout, before the 30s wait delay: "+
			"the descendant holding stdout was not killed with the CLI (GitHub #369)", name)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("%s: the fake CLI recorded no descendant pid: %v", name, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("%s: bad descendant pid %q", name, b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processRunning(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: descendant %d is still running after the call returned: "+
				"the timeout killed the CLI but not its process group (GitHub #369)", name, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processRunning reports whether pid is alive and not a zombie.
func processRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// The state field follows the parenthesised command name.
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

func TestCLIClient_ATimedOutCallKillsItsDescendant(t *testing.T) {
	bin := writeOrphaningCLI(t)
	c := NewCLIClient("model-x",
		WithCLIBinary(spawn.NewConfiguredCommand(bin)),
		WithCLITimeout(300*time.Millisecond))
	assertTimeoutKillsDescendant(t, "claude cli", filepath.Join(filepath.Dir(bin), "child.pid"), func() error {
		_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
		return err
	})
}

func TestCodexCLIClient_ATimedOutCallKillsItsDescendant(t *testing.T) {
	bin := writeOrphaningCLI(t)
	c := NewCodexCLIClient("model-x",
		WithCodexBinary(spawn.NewConfiguredCommand(bin)),
		WithCodexTimeout(300*time.Millisecond))
	assertTimeoutKillsDescendant(t, "codex cli", filepath.Join(filepath.Dir(bin), "child.pid"), func() error {
		_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
		return err
	})
}

func TestCodexCLIClient_ADescendantHoldingStdoutDoesNotHangTheCall(t *testing.T) {
	withShortCLIWaitDelay(t)
	bin := writeOrphaningCLI(t)
	c := NewCodexCLIClient("model-x",
		WithCodexBinary(spawn.NewConfiguredCommand(bin)),
		WithCodexTimeout(300*time.Millisecond))
	assertReturnsAsTimeout(t, "codex cli", func() error {
		_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
		return err
	})
}

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

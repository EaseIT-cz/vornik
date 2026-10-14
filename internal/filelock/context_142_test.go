package filelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdLock takes the flock on path through its own open file description, as
// a sibling process would, and releases it at cleanup.
func holdLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

// TestWithExclusiveContext_GivesUpAndRefusesWhenTheContextEnds — audit
// 2026-10-12, GitHub #142 §S7 #10: the credential-refresh flock was a blocking
// LOCK_EX with no bound, so a holder that hung blocked the refresh forever.
// The wait now ends with the context, and fn does not run unlocked.
func TestWithExclusiveContext_GivesUpAndRefusesWhenTheContextEnds(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "creds.lock")
	holdLock(t, lock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ran := false
	done := make(chan error, 1)
	go func() { done <- WithExclusiveContext(ctx, lock, func() error { ran = true; return nil }) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), lock) {
			t.Errorf("err %q does not name the lock file", err)
		}
		if ran {
			t.Error("fn ran without the lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WithExclusiveContext is still waiting: the lock wait ignores its context")
	}
}

// TestWithExclusiveContext_TakesAFreeLockAndOneThatIsReleased — the ordinary
// paths: a free lock runs fn at once; a lock released while waiting is taken.
func TestWithExclusiveContext_TakesAFreeLockAndOneThatIsReleased(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "creds.lock")
	ran := false
	if err := WithExclusiveContext(context.Background(), lock, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("free lock: err=%v ran=%v", err, ran)
	}

	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithExclusiveContext(context.Background(), lock, func() error { close(entered); return nil })
	}()
	select {
	case <-entered:
		t.Fatal("fn ran while another holder had the lock")
	case <-time.After(50 * time.Millisecond):
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the released lock was never taken")
	}
}

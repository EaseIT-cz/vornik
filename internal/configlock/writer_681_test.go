package configlock

import (
	"context"
	"errors"
	"testing"
)

// GitHub #681 (operations-api-contract design §6.10, operator decision
// O-676-1): an interactive config writer on a daemon that does not hold the
// config_writer lease is refused, and the lock is left free.
func TestLockAsWriter_681(t *testing.T) {
	t.Cleanup(func() { SetWriterCheck(nil) })

	// No check registered: as today, it locks.
	unlock, err := LockAsWriter(context.Background())
	if err != nil {
		t.Fatalf("no check: %v", err)
	}
	unlock()

	SetWriterCheck(func(context.Context) (bool, string) { return false, "lease held by node-b" })
	if _, err := LockAsWriter(context.Background()); !errors.Is(err, ErrNotConfigWriter) {
		t.Fatalf("refusing check: err = %v, want ErrNotConfigWriter", err)
	}
	release, ok := TryLock()
	if !ok {
		t.Fatal("a refused LockAsWriter must leave the lock free")
	}
	release()

	SetWriterCheck(func(context.Context) (bool, string) { return true, "" })
	unlock, err = LockAsWriter(context.Background())
	if err != nil {
		t.Fatalf("allowing check: %v", err)
	}
	if _, ok := TryLock(); ok {
		t.Fatal("LockAsWriter must hold the lock")
	}
	unlock()
}

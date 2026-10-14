// Package filelock serialises a critical section across goroutines AND
// processes with an exclusive advisory flock on a dedicated lock file.
//
// A separate lock file (never renamed) is used rather than the protected file
// itself, because writers replace the protected file via temp+rename, which
// would drop a lock held on the old inode. Best-effort: if the lock cannot be
// taken (read-only directory, unsupported filesystem) fn still runs — the
// callers' own writes would fail in the same conditions, and refusing would
// turn a lock problem into a data problem.
//
// Extracted from chat's credential-refresh lock (2026-06 codex hardening) when
// the config_template_drift acknowledgement store needed the same guarantee:
// one implementation of the serialisation, not two.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// WithExclusive runs fn while holding an exclusive flock on lockPath.
func WithExclusive(lockPath string, fn func() error) error {
	if lockPath == "" {
		return fn()
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn()
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// Exclusive runs fn while holding an exclusive flock on lockPath, and REFUSES
// (returns the error, fn not run) when the lock cannot be taken. For callers
// whose whole correctness is the serialisation — the config_template_drift
// ack store's read-modify-write, where running unlocked on a filesystem that
// accepts writes but not flock would silently lose concurrent records (drift
// design, round 12 F3). WithExclusive keeps its best-effort contract for
// callers that prefer running to refusing.
func Exclusive(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// ErrLockBusy is what WithExclusiveContext's refusal wraps: another holder
// kept the lock until the context ended, so fn did not run.
var ErrLockBusy = errors.New("lock held by another process")

// WithExclusiveContext runs fn while holding an exclusive flock on lockPath,
// waiting for a lock another holder has no longer than ctx allows
// (outcome-quality LLD §S7 #10, GitHub #142). It retries a non-blocking lock
// every 25ms, doubling to 250ms. When ctx ends first it REFUSES: it returns an
// error naming the lock file and wrapping ctx.Err(), and fn does not run —
// the caller's serialisation is the point. Any other failure to open or lock
// the file keeps WithExclusive's best-effort contract (fn runs).
func WithExclusiveContext(ctx context.Context, lockPath string, fn func() error) error {
	if lockPath == "" {
		return fn()
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn()
	}
	defer func() { _ = f.Close() }()
	wait := 25 * time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fn()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: waiting for the lock %s, which another process holds: %w", ErrLockBusy, lockPath, ctx.Err())
		case <-t.C:
		}
		if wait < 250*time.Millisecond {
			wait *= 2
		}
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

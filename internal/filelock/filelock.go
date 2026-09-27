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
	"os"
	"syscall"
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

package chat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vornik.io/vornik/internal/filelock"
)

// authLockWaitTimeout bounds the wait for a sibling process's credential
// refresh lock (outcome-quality LLD §S7 #10, GitHub #142): the holder's
// refresh is bounded by its own 30s HTTP client plus one file write.
const authLockWaitTimeout = 45 * time.Second

// withAuthFileLock runs fn while holding an exclusive advisory lock on
// a sibling "<path>.lock" file, serializing credential refreshes across
// processes that share the same auth/credentials file. A separate lock
// file (never renamed) is used rather than the credentials file itself
// because the save paths replace the credentials inode via temp+rename,
// which would drop a lock held on the old fd. Best-effort: if the lock
// can't be taken (read-only dir, etc.) we degrade to in-process locking
// rather than failing the refresh.
//
// The wait for a lock another process holds is bounded by ctx and by
// authLockWaitTimeout (outcome-quality LLD §S7 #10). When the wait runs out
// the refresh is refused, never run unlocked: an unlocked refresh risks the
// single-use refresh-token race the lock exists for, which costs a re-login.
//
// Extracted from the codex manager's withFileLock (the 2026-06 codex
// single-use-refresh-token hardening) so the claude manager — which
// shares ~/.claude/.credentials.json with the interactive CLI and any
// sibling vornik process — uses the identical serialization
// (2026-06-07 architecture review, subscription-auth finding 2). The lock
// itself now lives in internal/filelock, shared with the
// config_template_drift acknowledgement store.
func withAuthFileLock(ctx context.Context, path string, fn func() error) error {
	if path == "" {
		return fn()
	}
	wctx, cancel := context.WithTimeout(ctx, authLockWaitTimeout)
	defer cancel()
	return filelock.WithExclusiveContext(wctx, path+".lock", fn)
}

// ctxMutex is a mutex whose waiters can give up when their context ends
// (outcome-quality LLD §S7 #10). The subscription auth managers hold it
// across a token refresh, and every model call through the provider waits
// for it; with a sync.Mutex those calls could not give up. The zero value is
// unlocked and ready to use.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (l *ctxMutex) slot() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

// Lock waits for the lock with no bound.
func (l *ctxMutex) Lock() { l.slot() <- struct{}{} }

// Unlock releases the lock. Unlocking an unlocked ctxMutex panics, as it does
// for a sync.Mutex: a receive from the empty slot would otherwise block the
// caller forever, and with it every later Token call (code review b20f F1).
func (l *ctxMutex) Unlock() {
	select {
	case <-l.slot():
	default:
		panic("chat: unlock of unlocked ctxMutex")
	}
}

// LockContext waits for the lock until ctx ends; what is waiting names the
// lock in the error.
func (l *ctxMutex) LockContext(ctx context.Context, what string) error {
	select {
	case l.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the %s: %w", what, ctx.Err())
	}
}

// Package configlock holds the process's one config-write lock (GitHub #584,
// operations-api-contract-design.md §6.6). Every writer of the daemon's
// config files holds it across its read, compute, backup, write and
// validate: the setup and operations handlers, the feature-doctor enable,
// the integration save, the project archive and the control-plane apply. It
// is a leaf package so that any of them can import it. Before it, two
// separate mutexes (one in api, one in controlplane) each guarded some
// writers and others took none.
//
// The lock is per process. Another daemon on the same files is fenced by
// the config-writer lease (config-apply-journal design §10.6).
package configlock

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var mu sync.Mutex

// Lock blocks until the lock is held and returns its release.
func Lock() (unlock func()) {
	mu.Lock()
	return mu.Unlock
}

// TryLock takes the lock if it is free. When it is held elsewhere it
// returns false and a no-op release: the control plane refuses a busy apply
// rather than queueing it.
func TryLock() (unlock func(), ok bool) {
	if !mu.TryLock() {
		return func() {}, false
	}
	return mu.Unlock, true
}

// ErrNotConfigWriter is returned by LockAsWriter on a daemon that does not
// hold the config_writer lease: nothing may be written here (GitHub #681,
// operations-api-contract design §6.10).
var ErrNotConfigWriter = errors.New("configlock: this daemon is not the config writer; make the change on the daemon that holds the config_writer lease")

var (
	checkMu     sync.RWMutex
	writerCheck func(context.Context) (bool, string)
)

// SetWriterCheck registers the check LockAsWriter asks after taking the lock
// (the service passes leaderelection.DangerousWriteAllowed over the
// config-writer elector). A function keeps this a leaf package. nil removes it.
func SetWriterCheck(f func(context.Context) (allowed bool, reason string)) {
	checkMu.Lock()
	defer checkMu.Unlock()
	writerCheck = f
}

// LockAsWriter takes the lock, then asks the writer check. On a refusal it
// releases the lock and returns ErrNotConfigWriter wrapping the reason. With
// no check registered it allows, as Lock does. Interactive writers use it;
// the control plane and workflowapply keep their own fences.
func LockAsWriter(ctx context.Context) (unlock func(), err error) {
	mu.Lock()
	checkMu.RLock()
	f := writerCheck
	checkMu.RUnlock()
	if f != nil {
		if ok, reason := f(ctx); !ok {
			mu.Unlock()
			return nil, fmt.Errorf("%w (%s)", ErrNotConfigWriter, reason)
		}
	}
	return mu.Unlock, nil
}

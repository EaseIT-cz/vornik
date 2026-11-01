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

import "sync"

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

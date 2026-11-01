package featuredoctor

import (
	"vornik.io/vornik/internal/configlock"
)

// LockConfigFile acquires the process's one config-write lock
// (internal/configlock) and returns its release func. EVERY read-modify-write
// transaction against a config file MUST hold it across the full
// backup→read→patch→write→validate→reload sequence: FileConfigWriter's
// temp+rename stops file CORRUPTION, not a LOST UPDATE (two transactions each
// read the old file, patch their own copy, and the second write clobbers the
// first's just-landed keys).
//
// It was path-keyed until GitHub #584 (operations-api-contract-design.md §6.6):
// a separate lock from the operations' and the control plane's, so writers
// holding different locks still interleaved on one file. Now every writer
// holds the same lock, whatever the path.
//
// Process-local only. Another daemon on the same files is fenced by the
// config-writer lease.
func LockConfigFile(path string) (unlock func()) {
	// The process's one config-write lock (GitHub #584,
	// operations-api-contract-design.md §6.6): every writer of the config
	// files holds it, so this serialises with rotation, setup, the archive
	// and the control plane too, not only with writers of the same path.
	// path stays in the signature for its callers; the lock no longer keys
	// on it.
	_ = path
	return configlock.Lock()
}

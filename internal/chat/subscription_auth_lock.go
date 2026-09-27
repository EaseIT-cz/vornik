package chat

import "vornik.io/vornik/internal/filelock"

// withAuthFileLock runs fn while holding an exclusive advisory lock on
// a sibling "<path>.lock" file, serializing credential refreshes across
// processes that share the same auth/credentials file. A separate lock
// file (never renamed) is used rather than the credentials file itself
// because the save paths replace the credentials inode via temp+rename,
// which would drop a lock held on the old fd. Best-effort: if the lock
// can't be taken (read-only dir, etc.) we degrade to in-process locking
// rather than failing the refresh.
//
// Extracted from the codex manager's withFileLock (the 2026-06 codex
// single-use-refresh-token hardening) so the claude manager — which
// shares ~/.claude/.credentials.json with the interactive CLI and any
// sibling vornik process — uses the identical serialization
// (2026-06-07 architecture review, subscription-auth finding 2). The lock
// itself now lives in internal/filelock, shared with the
// config_template_drift acknowledgement store.
func withAuthFileLock(path string, fn func() error) error {
	if path == "" {
		return fn()
	}
	return filelock.WithExclusive(path+".lock", fn)
}

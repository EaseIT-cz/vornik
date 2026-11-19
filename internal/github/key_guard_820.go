package github

// GitHub #820 (github-app-channel design, amendment 2026-11-19, change 4):
// the #812 check, run at every mint. A hand-made block may not mint with the
// instance App key's bytes; the registry refuses such a block at load, and
// this guard closes the window between a load that passed and the next
// reload (an instance key that first appears after a site was built).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
)

// ErrInstanceKeyRefused refuses a mint by a hand-made block with the
// instance App key.
var ErrInstanceKeyRefused = errors.New("github-app: this block's key is the instance's App key; a hand-made block may not use it (use use_registration)")

var instanceKeyPath atomic.Value // string

// SetInstanceAppKeyPath names the instance App key file the guard compares
// against; "" turns the guard off. The service sets it at boot beside
// registry.SetInstanceAppKeyPath.
func SetInstanceAppKeyPath(path string) { instanceKeyPath.Store(path) }

// KeyHash is the SHA-256 of key bytes, hex.
func KeyHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// GuardKey refuses keyHash when it is the instance App key's and the block is
// hand-made. The instance key is read at call time, so a key registered after
// the site was built is seen at its next mint. No instance key (none
// registered, or unreadable) refuses nothing, as #812's load check does.
func GuardKey(keyHash string, appBacked bool) error {
	if appBacked || keyHash == "" {
		return nil
	}
	path, _ := instanceKeyPath.Load().(string)
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if KeyHash(b) == keyHash {
		return ErrInstanceKeyRefused
	}
	return nil
}

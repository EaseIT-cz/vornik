package integrations

import (
	"io"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// The dial guard (design §6, integrations-hub-design §0.0b) lives in the leaf
// package internal/integrations/dialguard since GitHub #465, so that the
// executor, autonomy and service packages can dial through it: this package
// imports the executor, and the guard's callers there could not import it.
// These names are aliases of the one implementation, not a second one.

// DialGuard is dialguard.DialGuard (see that type for the contract).
type DialGuard = dialguard.DialGuard

// ErrDialRefused is dialguard.ErrDialRefused: the same error value, so
// errors.Is matches whichever name a caller uses.
var ErrDialRefused = dialguard.ErrDialRefused

// IsLoopbackHost is dialguard.IsLoopbackHost.
func IsLoopbackHost(host string) bool { return dialguard.IsLoopbackHost(host) }

// LoopbackURLGuard is dialguard.LoopbackURLGuard.
func LoopbackURLGuard(rawURL string, allowed []string) DialGuard {
	return dialguard.LoopbackURLGuard(rawURL, allowed)
}

// ReadBoundedBody is dialguard.ReadBoundedBody.
func ReadBoundedBody(r io.Reader, limit int64) ([]byte, error) {
	return dialguard.ReadBoundedBody(r, limit)
}

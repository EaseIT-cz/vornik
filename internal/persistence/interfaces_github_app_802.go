package persistence

import (
	"context"
	"time"
)

// ManifestStateClass says why a GitHub App-manifest state can or cannot be
// consumed (GitHub #802). It labels the metric and the log; the consume's
// own single statement is the only gate.
type ManifestStateClass string

// The classes.
const (
	ManifestStateLive         ManifestStateClass = "live"
	ManifestStateMissing      ManifestStateClass = "state_missing"
	ManifestStateExpired      ManifestStateClass = "state_expired"
	ManifestStateUsed         ManifestStateClass = "state_used"
	ManifestStateOtherSession ManifestStateClass = "state_other_session"
)

// GitHubAppManifestStateRepository holds the console's App-manifest states
// (migration 256; github-app-channel design, "One-click App creation",
// Data). Both arguments are hashes: the raw state and session id are never
// stored.
type GitHubAppManifestStateRepository interface {
	// PutManifestState deletes the session's unused states, prunes rows a
	// day past their expiry, and inserts the new state with the GitHub REST
	// base its conversion will call (github.com or a GitHub Enterprise host).
	PutManifestState(ctx context.Context, stateHash, sessionHash, apiBase string, now, expiresAt time.Time) error
	// ClassifyManifestState reads the row for the metric's label.
	ClassifyManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (ManifestStateClass, error)
	// ConsumeManifestState marks the state used in one statement, only if it
	// is the session's, unused and unexpired, and returns its REST base;
	// ok false means refused.
	ConsumeManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (apiBase string, ok bool, err error)
}

// ClassifyManifestRow is the one classification rule both drivers apply to
// a row they read (found false: no row).
func ClassifyManifestRow(found bool, rowSession, session string, used bool, expiresAt, now time.Time) ManifestStateClass {
	switch {
	case !found:
		return ManifestStateMissing
	case rowSession != session:
		return ManifestStateOtherSession
	case used:
		return ManifestStateUsed
	case !now.Before(expiresAt):
		return ManifestStateExpired
	}
	return ManifestStateLive
}

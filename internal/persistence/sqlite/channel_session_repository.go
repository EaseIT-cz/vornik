package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// ChannelSessionRepository is the SQLite stub. Single-process
// deployments keep their in-memory map as the authoritative
// state, so the repo presents itself as "always empty": Load
// returns ErrNotFound (channels fall back to their in-memory
// cache), SaveIfVersion is a no-op (the cache already holds the same
// data), Delete is a no-op.
//
// Wiring the interface uniformly across both backends means the
// channel implementations never branch on backend — they just call
// Load/SaveIfVersion and let the repo's semantics decide whether the call
// is durable.
type ChannelSessionRepository struct {
	_ *sql.DB
}

// NewChannelSessionRepository returns the stub.
func NewChannelSessionRepository(db *sql.DB) *ChannelSessionRepository {
	return &ChannelSessionRepository{}
}

// Load always returns ErrNotFound. Channels treat that the same
// as "session not in DB yet"; their in-memory cache supplies the
// history.
func (r *ChannelSessionRepository) Load(_ context.Context, _, _ string) (*persistence.ChannelSession, error) {
	return nil, persistence.ErrNotFound
}

// Version reports no row: nothing is stored on SQLite.
func (r *ChannelSessionRepository) Version(_ context.Context, _, _ string) (bool, int64, error) {
	return false, 0, nil
}

// SaveIfVersion is a no-op that reports success: the in-memory map is the
// only state.
func (r *ChannelSessionRepository) SaveIfVersion(_ context.Context, _, _, _ string, _ []byte, _ *int64) (int64, bool, error) {
	return 0, true, nil
}

// Fenced is false: there is nothing durable to compare against, and no
// erasure of a channel session runs on SQLite (art17 redact design §15.18).
func (r *ChannelSessionRepository) Fenced() bool { return false }

// Delete is a no-op for the same reason as SaveIfVersion.
func (r *ChannelSessionRepository) Delete(_ context.Context, _, _ string) error {
	return nil
}

// ListByPrefix always returns no rows, consistent with Load's ErrNotFound:
// this backend stores nothing, so it has no sibling sessions to report.
// Callers that use it to enrich context (the Slack channel's thread digests)
// degrade to their in-memory view rather than erroring.
func (r *ChannelSessionRepository) ListByPrefix(_ context.Context, _, _ string, _ int) ([]*persistence.ChannelSession, error) {
	return nil, nil
}

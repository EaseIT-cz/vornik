package persistence

import (
	"context"
	"time"
)

// ChannelSession is one row in the `channel_sessions` table —
// the per-channel conversation snapshot that survives daemon
// restarts and lets a replica pick up a conversation the
// originating replica started.
//
// History is the JSON-encoded chat.Message slice. The
// persistence layer holds raw bytes (not chat.Message) so
// channel_session.go doesn't need to import internal/chat —
// which already imports persistence, so the dependency must run
// in one direction. Callers marshal at their boundary.
//
// Channels:
//   - webchat: SessionID = cookie hash
//   - email:   SessionID = RFC822 thread root message-id
//   - slack:   SessionID = channel.thread_ts
//   - github:  SessionID = "owner/repo#issues/N" / "...#pulls/N"
//   - telegram (future phase): SessionID = chat_id (numeric, stringified)
type ChannelSession struct {
	Kind          string
	SessionID     string
	ActiveProject string
	History       []byte // JSON-encoded []chat.Message
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     *time.Time
	// Version is the row's fence version (Postgres; 0 on SQLite). Every
	// write draws a new one from one sequence, so a value is never reused.
	Version int64
}

// ChannelSessionRepository persists per-channel session state.
// Implementations:
//   - Postgres: durable, multi-replica safe, and fenced: every write
//     draws a new version and SaveIfVersion writes only over the
//     version the caller last saw (art17 redact design §15.18, GitHub
//     #344), so a session an erasure deleted or masked is never
//     written back from a node's memory.
//   - SQLite: returns ErrNotFound on Load and is a no-op on writes —
//     single-process deployments keep their in-memory map as the
//     authoritative state, and Fenced reports false.
//
// There is no unconditional save: a blind upsert is what let a deleted
// session come back.
type ChannelSessionRepository interface {
	// Load returns the session for (kind, sessionID). ErrNotFound
	// for a session that has never been persisted — channels treat
	// that the same as "fresh session, empty history".
	Load(ctx context.Context, kind, sessionID string) (*ChannelSession, error)

	// Version reads the row's version alone; found is false when the
	// row does not exist.
	Version(ctx context.Context, kind, sessionID string) (found bool, version int64, err error)

	// SaveIfVersion writes the session only when the row still holds
	// expect, or, with expect nil, only when no row exists. ok is false
	// (and nothing is written) when the row moved; newVersion is the
	// version the write drew. activeProject may be empty. historyJSON is
	// the JSON-encoded full post-turn slice.
	SaveIfVersion(ctx context.Context, kind, sessionID, activeProject string, historyJSON []byte, expect *int64) (newVersion int64, ok bool, err error)

	// Fenced reports whether this store carries versions. False on the
	// SQLite stub, where there is nothing durable to compare.
	Fenced() bool

	// Delete removes the session. Used by the webchat "clear chat"
	// affordance and the future stale-session sweeper. No error
	// when the row doesn't exist — idempotent.
	Delete(ctx context.Context, kind, sessionID string) error

	// ListByPrefix returns sessions whose SessionID starts with prefix,
	// most-recently-updated first, capped at limit (<=0 yields no rows).
	//
	// Exists because a SessionID encodes its own container: every Slack
	// thread in one channel shares the prefix "<team>/<channel>#", so
	// "the other conversations in this channel" is a prefix query rather
	// than a new data model. Used to surface what was discussed in a
	// channel's threads to a channel-level turn — Slack threads are
	// unfindable days later, so users follow up in the channel instead.
	//
	// prefix is matched literally: implementations MUST escape LIKE
	// metacharacters so a session id containing '%' or '_' cannot widen
	// the match.
	ListByPrefix(ctx context.Context, kind, prefix string, limit int) ([]*ChannelSession, error)
}

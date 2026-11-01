package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ChannelSessionRepository implements
// persistence.ChannelSessionRepository against PostgreSQL. The
// (kind, session_id) composite PK keeps webchat/email/slack/github
// namespaces isolated within a single table. Writes are fenced on the
// row's version (migration 241, art17 redact design §15.18).
type ChannelSessionRepository struct {
	db DBTX
}

// NewChannelSessionRepository constructs the repo over db.
func NewChannelSessionRepository(db DBTX) *ChannelSessionRepository {
	return &ChannelSessionRepository{db: db}
}

// Load returns the session row. ErrNotFound when no row exists —
// callers treat that as a fresh empty session.
//
// History is returned as the raw JSONB bytes; the caller
// unmarshals into []chat.Message at the channel boundary so this
// package doesn't import internal/chat.
func (r *ChannelSessionRepository) Load(ctx context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	if kind == "" || sessionID == "" {
		return nil, fmt.Errorf("channel_session: kind + sessionID required")
	}
	const q = `
SELECT kind, session_id, COALESCE(active_project, ''), history, created_at, updated_at, expires_at, version
FROM channel_sessions
WHERE kind = $1 AND session_id = $2`
	row := r.db.QueryRowContext(ctx, q, kind, sessionID)
	var (
		s         persistence.ChannelSession
		expiresAt sql.NullTime
	)
	if err := row.Scan(&s.Kind, &s.SessionID, &s.ActiveProject, &s.History, &s.CreatedAt, &s.UpdatedAt, &expiresAt, &s.Version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("channel_session: load: %w", err)
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		s.ExpiresAt = &t
	}
	return &s, nil
}

// Version reads the row's version alone, one primary-key read per channel
// turn (the fence's freshness check).
func (r *ChannelSessionRepository) Version(ctx context.Context, kind, sessionID string) (bool, int64, error) {
	var v int64
	err := r.db.QueryRowContext(ctx,
		`SELECT version FROM channel_sessions WHERE kind = $1 AND session_id = $2`, kind, sessionID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("channel_session: version: %w", err)
	}
	return true, v, nil
}

// SaveIfVersion writes the session only over the version the caller saw:
// with expect nil, only while no row exists; otherwise only while the row
// still holds *expect. The trigger from migration 241 draws the new version.
// A refused write changes nothing and reports ok=false.
func (r *ChannelSessionRepository) SaveIfVersion(ctx context.Context, kind, sessionID, activeProject string, historyJSON []byte, expect *int64) (int64, bool, error) {
	if kind == "" || sessionID == "" {
		return 0, false, fmt.Errorf("channel_session: kind + sessionID required")
	}
	// Empty payload becomes the JSONB empty array so the column's
	// NOT NULL constraint is satisfied.
	if len(historyJSON) == 0 {
		historyJSON = []byte("[]")
	}
	var ap sql.NullString
	if activeProject != "" {
		ap = sql.NullString{String: activeProject, Valid: true}
	}
	var row *sql.Row
	if expect == nil {
		row = r.db.QueryRowContext(ctx, `
INSERT INTO channel_sessions (kind, session_id, active_project, history, created_at, updated_at)
VALUES ($1, $2, $3, $4, NOW(), NOW())
ON CONFLICT (kind, session_id) DO NOTHING
RETURNING version`, kind, sessionID, ap, historyJSON)
	} else {
		row = r.db.QueryRowContext(ctx, `
UPDATE channel_sessions SET active_project = $3, history = $4, updated_at = NOW()
WHERE kind = $1 AND session_id = $2 AND version = $5
RETURNING version`, kind, sessionID, ap, historyJSON, *expect)
	}
	var v int64
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("channel_session: save: %w", err)
	}
	return v, true, nil
}

// Fenced is true: the Postgres table carries versions.
func (r *ChannelSessionRepository) Fenced() bool { return true }

// ListByPrefix returns the sessions under a SessionID prefix, newest activity
// first. See the interface doc for why a prefix query is the right shape.
//
// The prefix is escaped before interpolation into LIKE: a Slack channel id
// can't contain '%' or '_', but the argument is not this package's to trust,
// and an unescaped '%' would silently widen the match to other channels — the
// containment property the caller relies on.
func (r *ChannelSessionRepository) ListByPrefix(ctx context.Context, kind, prefix string, limit int) ([]*persistence.ChannelSession, error) {
	if kind == "" || prefix == "" {
		return nil, fmt.Errorf("channel_session: kind + prefix required")
	}
	if limit <= 0 {
		return nil, nil
	}
	const q = `
SELECT kind, session_id, COALESCE(active_project, ''), history, created_at, updated_at, expires_at, version
FROM channel_sessions
WHERE kind = $1 AND session_id LIKE $2
ORDER BY updated_at DESC
LIMIT $3`
	rows, err := r.db.QueryContext(ctx, q, kind, escapeLikePrefix(prefix)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("channel_session: list by prefix: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.ChannelSession
	for rows.Next() {
		var (
			s         persistence.ChannelSession
			expiresAt sql.NullTime
		)
		if err := rows.Scan(&s.Kind, &s.SessionID, &s.ActiveProject, &s.History, &s.CreatedAt, &s.UpdatedAt, &expiresAt, &s.Version); err != nil {
			return nil, fmt.Errorf("channel_session: list by prefix scan: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			s.ExpiresAt = &t
		}
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("channel_session: list by prefix rows: %w", err)
	}
	return out, nil
}

// Delete removes the session. Idempotent — no error when the row
// doesn't exist; the caller's "clear chat" affordance shouldn't
// fail for a session that was never persisted.
func (r *ChannelSessionRepository) Delete(ctx context.Context, kind, sessionID string) error {
	const q = `DELETE FROM channel_sessions WHERE kind = $1 AND session_id = $2`
	if _, err := r.db.ExecContext(ctx, q, kind, sessionID); err != nil {
		return fmt.Errorf("channel_session: delete: %w", err)
	}
	return nil
}

package subjectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// The store side of slice 5d-2 (GitHub #344): a channel session erased
// through the version fence. Every statement is guarded on the version the
// erasure read; the trigger from migration 241 draws a new one on a write.
// Postgres only (Dialect.ChannelSessions).
//
// see LLD § https://docs.vornik.io §15.18

// ChannelSessionEraser returns the 5d-2 store, or nil where the driver keeps
// no channel sessions (SQLite: the daemon's memory only).
func (r *Repository) ChannelSessionEraser() datasubject.ChannelSessionStore {
	if !r.d.ChannelSessions {
		return nil
	}
	return channelSessionEraser{r: r}
}

type channelSessionEraser struct{ r *Repository }

// LoadSession resolves the link row id and loads the row and its version.
func (c channelSessionEraser) LoadSession(ctx context.Context, rowID string) (datasubject.ChannelSession, bool, error) {
	kind, sid, found, err := c.r.channelSessionKey(ctx, rowID)
	if err != nil || !found {
		return datasubject.ChannelSession{}, false, err
	}
	s := datasubject.ChannelSession{Kind: kind, SessionID: sid}
	var history string
	err = c.r.db.QueryRowContext(ctx,
		`SELECT history::text, version FROM channel_sessions WHERE kind = $1 AND session_id = $2`,
		kind, sid).Scan(&history, &s.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return datasubject.ChannelSession{}, false, nil
	}
	if err != nil {
		return datasubject.ChannelSession{}, false, fmt.Errorf("subjectstore: load channel session %s/%s: %w", kind, sid, err)
	}
	s.History = []byte(history)
	return s, true, nil
}

// DeleteSession deletes the row only while it still holds s.Version.
func (c channelSessionEraser) DeleteSession(ctx context.Context, s datasubject.ChannelSession) (bool, error) {
	res, err := c.r.db.ExecContext(ctx,
		`DELETE FROM channel_sessions WHERE kind = $1 AND session_id = $2 AND version = $3`,
		s.Kind, s.SessionID, s.Version)
	if err != nil {
		return false, fmt.Errorf("subjectstore: delete channel session %s/%s: %w", s.Kind, s.SessionID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// WriteSession replaces the history only while the row still holds s.Version.
func (c channelSessionEraser) WriteSession(ctx context.Context, s datasubject.ChannelSession, history []byte) (bool, error) {
	res, err := c.r.db.ExecContext(ctx,
		`UPDATE channel_sessions SET history = $3::jsonb, updated_at = $4
		 WHERE kind = $1 AND session_id = $2 AND version = $5`,
		s.Kind, s.SessionID, string(history), c.r.d.bindTime(c.r.now()), s.Version)
	if err != nil {
		return false, fmt.Errorf("subjectstore: write channel session %s/%s: %w", s.Kind, s.SessionID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FenceLaggards names what could write an erased session back: the schema
// when migration 241 is missing (no fenced daemon has started on this
// database), and every registered node that does not advertise the fence.
// A relay-only (DMZ) node holds no sessions and is not counted.
func (c channelSessionEraser) FenceLaggards(ctx context.Context) ([]string, error) {
	var hasColumn bool
	if err := c.r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'channel_sessions' AND column_name = 'version')`).
		Scan(&hasColumn); err != nil {
		return nil, fmt.Errorf("subjectstore: check the session fence column: %w", err)
	}
	if !hasColumn {
		return []string{"the database (migration 241, the session fence, is not applied, so no " +
			"fenced daemon has started on it)"}, nil
	}
	rows, err := c.r.db.QueryContext(ctx, `SELECT instance_id, version, capabilities::text FROM cluster_nodes`)
	if err != nil {
		return nil, fmt.Errorf("subjectstore: list cluster nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, version, capsJSON string
		if err := rows.Scan(&id, &version, &capsJSON); err != nil {
			return nil, fmt.Errorf("subjectstore: list cluster nodes: %w", err)
		}
		var caps map[string]bool
		if err := json.Unmarshal([]byte(capsJSON), &caps); err != nil {
			caps = nil // unreadable capabilities: treat as not fenced
		}
		if caps[persistence.ChannelSessionFenceCapability] || caps["RelayMode"] {
			continue
		}
		out = append(out, fmt.Sprintf("node %s (version %s)", id, version))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subjectstore: list cluster nodes: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

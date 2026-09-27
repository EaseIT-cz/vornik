package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ChannelDisclosureRepository is the SQLite implementation of the EU AI Act
// Art 50 disclosure record (migration 139).
//
// Note this is a REAL implementation, not the no-op stub pattern that
// ChannelSessionRepository uses on this backend. Session history can be
// no-opped on sqlite because an in-memory cache is authoritative; the
// disclosure record cannot, because it is the Art 99 evidence trail. A stub
// here would leave every single-node deployment unable to prove it disclosed.
type ChannelDisclosureRepository struct {
	db *sql.DB
}

// NewChannelDisclosureRepository builds the repository.
func NewChannelDisclosureRepository(db *sql.DB) *ChannelDisclosureRepository {
	return &ChannelDisclosureRepository{db: db}
}

var _ persistence.ChannelDisclosureRepository = (*ChannelDisclosureRepository)(nil)

// WasServed reports whether this (channel, session) has been disclosed to.
func (r *ChannelDisclosureRepository) WasServed(ctx context.Context, channel, sessionID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM channel_disclosure_log
		  WHERE channel = ? AND session_id = ?`,
		channel, sessionID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: channel_disclosure_log count: %w", err)
	}
	return n > 0, nil
}

// MarkServed records the disclosure, idempotently.
func (r *ChannelDisclosureRepository) MarkServed(ctx context.Context, channel, sessionID, textHash string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO channel_disclosure_log (channel, session_id, served_at, text_hash)
		      VALUES (?, ?, ?, ?)
		 ON CONFLICT (channel, session_id) DO NOTHING`,
		channel, sessionID, sqliteTime(time.Now()), textHash,
	)
	if err != nil {
		return fmt.Errorf("sqlite: channel_disclosure_log insert: %w", err)
	}
	return nil
}

// SummaryBetween aggregates in SQL per (channel, wording version) — the
// regulatory-record page's Art 50 evidence summary. served_at is RFC3339 text
// here, so MIN/MAX compare lexically, which orders correctly to the second
// (the same comparison the BETWEEN filter below already relies on).
func (r *ChannelDisclosureRepository) SummaryBetween(ctx context.Context, from, to time.Time) ([]persistence.ChannelDisclosureSummary, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT channel, text_hash, COUNT(*), MIN(served_at), MAX(served_at)
		   FROM channel_disclosure_log
		  WHERE served_at BETWEEN ? AND ?
		  GROUP BY channel, text_hash
		  ORDER BY channel, MIN(served_at)`,
		sqliteTime(from), sqliteTime(to))
	if err != nil {
		return nil, fmt.Errorf("sqlite: channel_disclosure_log summary: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []persistence.ChannelDisclosureSummary{}
	for rows.Next() {
		var (
			s           persistence.ChannelDisclosureSummary
			first, last string
		)
		if err := rows.Scan(&s.Channel, &s.TextHash, &s.Count, &first, &last); err != nil {
			return nil, fmt.Errorf("sqlite: channel_disclosure_log summary scan: %w", err)
		}
		if s.FirstServed, err = time.Parse(time.RFC3339Nano, first); err != nil {
			return nil, fmt.Errorf("sqlite: channel_disclosure_log first served %q: %w", first, err)
		}
		if s.LastServed, err = time.Parse(time.RFC3339Nano, last); err != nil {
			return nil, fmt.Errorf("sqlite: channel_disclosure_log last served %q: %w", last, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ServedBetween is the Art 99 enforcement-response query.
func (r *ChannelDisclosureRepository) ServedBetween(ctx context.Context, from, to time.Time) ([]persistence.ChannelDisclosure, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT channel, session_id, served_at, text_hash
		   FROM channel_disclosure_log
		  WHERE served_at BETWEEN ? AND ?
		  ORDER BY served_at`,
		sqliteTime(from), sqliteTime(to),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: channel_disclosure_log select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.ChannelDisclosure
	for rows.Next() {
		var (
			d        persistence.ChannelDisclosure
			servedAt string
		)
		if err := rows.Scan(&d.Channel, &d.SessionID, &servedAt, &d.TextHash); err != nil {
			return nil, fmt.Errorf("sqlite: channel_disclosure_log scan: %w", err)
		}
		ts, err := time.Parse(time.RFC3339Nano, servedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: channel_disclosure_log served_at %q: %w", servedAt, err)
		}
		d.ServedAt = ts
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: channel_disclosure_log rows: %w", err)
	}
	return out, nil
}

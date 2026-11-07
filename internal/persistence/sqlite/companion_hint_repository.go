package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CompanionHintRepository is the SQLite store of the companion hints' follow
// windows and last contact (mirrors postgres migration 248; GitHub #634
// slice 2, companion hardening design §13.10).
type CompanionHintRepository struct {
	db *sql.DB
}

// NewCompanionHintRepository builds the repository.
func NewCompanionHintRepository(db *sql.DB) *CompanionHintRepository {
	return &CompanionHintRepository{db: db}
}

var _ persistence.CompanionHintRepository = (*CompanionHintRepository)(nil)

const hintWindowCols = `hint_id, "window", backoff_until, fired_total, followed_total, rev, updated_at`

func scanHintWindow(s interface{ Scan(...any) error }, projectID string) (persistence.HintWindow, error) {
	w := persistence.HintWindow{ProjectID: projectID}
	var until sqlNullTime
	var updated sqlTime
	if err := s.Scan(&w.HintID, &w.Window, &until, &w.FiredTotal, &w.FollowedTotal, &w.Rev, &updated); err != nil {
		return w, err
	}
	if until.Valid {
		t := until.Time
		w.BackoffUntil = &t
	}
	w.UpdatedAt = updated.Time
	return w, nil
}

// Windows returns the project's rows, keyed by hint id.
func (r *CompanionHintRepository) Windows(ctx context.Context, projectID string) (map[string]persistence.HintWindow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+hintWindowCols+` FROM companion_hint_windows WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: companion_hint_windows list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]persistence.HintWindow{}
	for rows.Next() {
		w, err := scanHintWindow(rows, projectID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: companion_hint_windows scan: %w", err)
		}
		out[w.HintID] = w
	}
	return out, rows.Err()
}

// Apply applies events with persistence.ApplyHintEvents and a compare-and-set
// on rev (§13.10 S2).
func (r *CompanionHintRepository) Apply(ctx context.Context, projectID, hintID string, events []bool, now time.Time) (persistence.HintWindow, int, error) {
	for attempt := 0; attempt < persistence.HintWindowApplyRetries; attempt++ {
		cur, err := scanHintWindow(r.db.QueryRowContext(ctx,
			`SELECT `+hintWindowCols+` FROM companion_hint_windows WHERE project_id = ? AND hint_id = ?`,
			projectID, hintID), projectID)
		if errors.Is(err, sql.ErrNoRows) {
			cur = persistence.HintWindow{ProjectID: projectID, HintID: hintID}
		} else if err != nil {
			return persistence.HintWindow{}, 0, fmt.Errorf("sqlite: companion_hint_windows read: %w", err)
		}
		next, applied := persistence.ApplyHintEvents(cur, events, now)
		next.Rev = cur.Rev + 1
		var res sql.Result
		if cur.Rev == 0 {
			res, err = r.db.ExecContext(ctx,
				`INSERT INTO companion_hint_windows (project_id, `+hintWindowCols+`)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, hint_id) DO NOTHING`,
				projectID, hintID, next.Window, sqliteTimePtr(next.BackoffUntil), next.FiredTotal, next.FollowedTotal, next.Rev, sqliteTime(now))
		} else {
			res, err = r.db.ExecContext(ctx,
				`UPDATE companion_hint_windows SET "window" = ?, backoff_until = ?, fired_total = ?, followed_total = ?, rev = ?, updated_at = ?
				  WHERE project_id = ? AND hint_id = ? AND rev = ?`,
				next.Window, sqliteTimePtr(next.BackoffUntil), next.FiredTotal, next.FollowedTotal, next.Rev, sqliteTime(now),
				projectID, hintID, cur.Rev)
		}
		if err != nil {
			return persistence.HintWindow{}, 0, fmt.Errorf("sqlite: companion_hint_windows write: %w", err)
		}
		// No row affected: a concurrent upload moved rev, or the row went
		// with its project. Read again, which sees either (§13.10 S2).
		n, err := res.RowsAffected()
		if err != nil {
			// Not a conflict: a retry would double-count (review c48a F3).
			return persistence.HintWindow{}, 0, fmt.Errorf("sqlite: companion_hint_windows rows affected: %w", err)
		}
		if n == 1 {
			return next, applied, nil
		}
	}
	return persistence.HintWindow{}, 0, persistence.ErrHintWindowConflict
}

// TouchFetch records a key's last catalogue fetch.
func (r *CompanionHintRepository) TouchFetch(ctx context.Context, keyID, projectID string, at time.Time) error {
	return r.touch(ctx, "last_fetch_at", keyID, projectID, at)
}

// TouchUpload records a key's last outcome upload.
func (r *CompanionHintRepository) TouchUpload(ctx context.Context, keyID, projectID string, at time.Time) error {
	return r.touch(ctx, "last_upload_at", keyID, projectID, at)
}

// touch upserts one column; col is one of two constants above.
func (r *CompanionHintRepository) touch(ctx context.Context, col, keyID, projectID string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO companion_hint_clients (key_id, project_id, `+col+`) VALUES (?, ?, ?)
		 ON CONFLICT (key_id) DO UPDATE SET project_id = excluded.project_id, `+col+` = excluded.`+col,
		keyID, projectID, sqliteTime(at)); err != nil {
		return fmt.Errorf("sqlite: companion_hint_clients %s: %w", col, err)
	}
	return nil
}

// Clients returns the project's last-contact rows, by key id.
func (r *CompanionHintRepository) Clients(ctx context.Context, projectID string) ([]persistence.HintClient, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key_id, last_fetch_at, last_upload_at FROM companion_hint_clients WHERE project_id = ? ORDER BY key_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: companion_hint_clients list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.HintClient
	for rows.Next() {
		c := persistence.HintClient{ProjectID: projectID}
		var f, u sqlNullTime
		if err := rows.Scan(&c.KeyID, &f, &u); err != nil {
			return nil, fmt.Errorf("sqlite: companion_hint_clients scan: %w", err)
		}
		if f.Valid {
			t := f.Time
			c.LastFetchAt = &t
		}
		if u.Valid {
			t := u.Time
			c.LastUploadAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

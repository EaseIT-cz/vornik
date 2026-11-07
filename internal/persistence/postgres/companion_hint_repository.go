package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CompanionHintRepository is the Postgres store of the companion hints'
// follow windows and last contact (migration 248; GitHub #634 slice 2,
// companion hardening design §13.10).
type CompanionHintRepository struct {
	db persistence.DBTX
}

// NewCompanionHintRepository builds the repository.
func NewCompanionHintRepository(db persistence.DBTX) *CompanionHintRepository {
	return &CompanionHintRepository{db: db}
}

var _ persistence.CompanionHintRepository = (*CompanionHintRepository)(nil)

const hintWindowCols = `hint_id, "window", backoff_until, fired_total, followed_total, rev, updated_at`

func scanHintWindow(s interface{ Scan(...any) error }, projectID string) (persistence.HintWindow, error) {
	w := persistence.HintWindow{ProjectID: projectID}
	var until sql.NullTime
	if err := s.Scan(&w.HintID, &w.Window, &until, &w.FiredTotal, &w.FollowedTotal, &w.Rev, &w.UpdatedAt); err != nil {
		return w, err
	}
	if until.Valid {
		t := until.Time.UTC()
		w.BackoffUntil = &t
	}
	w.UpdatedAt = w.UpdatedAt.UTC()
	return w, nil
}

// Windows returns the project's rows, keyed by hint id.
func (r *CompanionHintRepository) Windows(ctx context.Context, projectID string) (map[string]persistence.HintWindow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+hintWindowCols+` FROM companion_hint_windows WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, fmt.Errorf("postgres: companion_hint_windows list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]persistence.HintWindow{}
	for rows.Next() {
		w, err := scanHintWindow(rows, projectID)
		if err != nil {
			return nil, fmt.Errorf("postgres: companion_hint_windows scan: %w", err)
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
			`SELECT `+hintWindowCols+` FROM companion_hint_windows WHERE project_id = $1 AND hint_id = $2`,
			projectID, hintID), projectID)
		if errors.Is(err, sql.ErrNoRows) {
			cur = persistence.HintWindow{ProjectID: projectID, HintID: hintID}
		} else if err != nil {
			return persistence.HintWindow{}, 0, fmt.Errorf("postgres: companion_hint_windows read: %w", err)
		}
		next, applied := persistence.ApplyHintEvents(cur, events, now)
		next.Rev = cur.Rev + 1
		var res sql.Result
		if cur.Rev == 0 {
			res, err = r.db.ExecContext(ctx,
				`INSERT INTO companion_hint_windows (project_id, `+hintWindowCols+`)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (project_id, hint_id) DO NOTHING`,
				projectID, hintID, next.Window, next.BackoffUntil, next.FiredTotal, next.FollowedTotal, next.Rev, now.UTC())
		} else {
			res, err = r.db.ExecContext(ctx,
				`UPDATE companion_hint_windows SET "window" = $1, backoff_until = $2, fired_total = $3, followed_total = $4, rev = $5, updated_at = $6
				  WHERE project_id = $7 AND hint_id = $8 AND rev = $9`,
				next.Window, next.BackoffUntil, next.FiredTotal, next.FollowedTotal, next.Rev, now.UTC(),
				projectID, hintID, cur.Rev)
		}
		if err != nil {
			return persistence.HintWindow{}, 0, fmt.Errorf("postgres: companion_hint_windows write: %w", err)
		}
		// No row affected: a concurrent upload moved rev, or the row went
		// with its project. Read again, which sees either (§13.10 S2).
		n, err := res.RowsAffected()
		if err != nil {
			// Not a conflict: a retry would double-count (review c48a F3).
			return persistence.HintWindow{}, 0, fmt.Errorf("postgres: companion_hint_windows rows affected: %w", err)
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
		`INSERT INTO companion_hint_clients (key_id, project_id, `+col+`) VALUES ($1, $2, $3)
		 ON CONFLICT (key_id) DO UPDATE SET project_id = EXCLUDED.project_id, `+col+` = EXCLUDED.`+col,
		keyID, projectID, at.UTC()); err != nil {
		return fmt.Errorf("postgres: companion_hint_clients %s: %w", col, err)
	}
	return nil
}

// Clients returns the project's last-contact rows, by key id.
func (r *CompanionHintRepository) Clients(ctx context.Context, projectID string) ([]persistence.HintClient, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key_id, last_fetch_at, last_upload_at FROM companion_hint_clients WHERE project_id = $1 ORDER BY key_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("postgres: companion_hint_clients list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.HintClient
	for rows.Next() {
		c := persistence.HintClient{ProjectID: projectID}
		var f, u sql.NullTime
		if err := rows.Scan(&c.KeyID, &f, &u); err != nil {
			return nil, fmt.Errorf("postgres: companion_hint_clients scan: %w", err)
		}
		if f.Valid {
			t := f.Time.UTC()
			c.LastFetchAt = &t
		}
		if u.Valid {
			t := u.Time.UTC()
			c.LastUploadAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

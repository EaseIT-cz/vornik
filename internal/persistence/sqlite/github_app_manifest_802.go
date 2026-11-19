package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHubAppManifestStateRepository is the SQLite driver of
// persistence.GitHubAppManifestStateRepository (GitHub #802). Times are the
// fixed-width SQLiteTime text, so text comparison orders them.
type GitHubAppManifestStateRepository struct{ db *sql.DB }

// NewGitHubAppManifestStateRepository wires the repository.
func NewGitHubAppManifestStateRepository(db *sql.DB) *GitHubAppManifestStateRepository {
	return &GitHubAppManifestStateRepository{db: db}
}

// PutManifestState implements the interface.
func (r *GitHubAppManifestStateRepository) PutManifestState(ctx context.Context, stateHash, sessionHash, apiBase string, now, expiresAt time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM github_app_manifest_states WHERE (session_hash = ? AND used_at IS NULL) OR expires_at < ?`,
		sessionHash, sqliteTime(now.Add(-24*time.Hour))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_app_manifest_states (state_hash, session_hash, api_base, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		stateHash, sessionHash, apiBase, sqliteTime(now), sqliteTime(expiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// ClassifyManifestState implements the interface.
func (r *GitHubAppManifestStateRepository) ClassifyManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (persistence.ManifestStateClass, error) {
	var session, expires string
	var used sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT session_hash, expires_at, used_at FROM github_app_manifest_states WHERE state_hash = ?`, stateHash).
		Scan(&session, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.ManifestStateMissing, nil
	}
	if err != nil {
		return "", err
	}
	exp, err := time.Parse(persistence.SQLiteTimeLayout, expires)
	if err != nil {
		return "", err
	}
	return persistence.ClassifyManifestRow(true, session, sessionHash, used.Valid, exp, now), nil
}

// ConsumeManifestState implements the interface. RETURNING keeps the
// mark and the read one statement.
func (r *GitHubAppManifestStateRepository) ConsumeManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (string, bool, error) {
	var apiBase string
	err := r.db.QueryRowContext(ctx, `UPDATE github_app_manifest_states SET used_at = ?
		WHERE state_hash = ? AND session_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING api_base`,
		sqliteTime(now), stateHash, sessionHash, sqliteTime(now)).Scan(&apiBase)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return apiBase, true, nil
}

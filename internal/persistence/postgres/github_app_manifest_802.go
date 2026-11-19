package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHubAppManifestStateRepository is the Postgres driver of
// persistence.GitHubAppManifestStateRepository (GitHub #802).
type GitHubAppManifestStateRepository struct{ db persistence.DBTX }

// NewGitHubAppManifestStateRepository wires the repository.
func NewGitHubAppManifestStateRepository(db persistence.DBTX) *GitHubAppManifestStateRepository {
	return &GitHubAppManifestStateRepository{db: db}
}

// PutManifestState implements the interface: one statement, so the
// supersede and the insert are atomic without a transaction handle.
func (r *GitHubAppManifestStateRepository) PutManifestState(ctx context.Context, stateHash, sessionHash, apiBase string, now, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `WITH gone AS (
		DELETE FROM github_app_manifest_states WHERE (session_hash = $2 AND used_at IS NULL) OR expires_at < $5
	)
	INSERT INTO github_app_manifest_states (state_hash, session_hash, api_base, created_at, expires_at) VALUES ($1, $2, $6, $3, $4)`,
		stateHash, sessionHash, now, expiresAt, now.Add(-24*time.Hour), apiBase)
	return mapDBError(err)
}

// ClassifyManifestState implements the interface.
func (r *GitHubAppManifestStateRepository) ClassifyManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (persistence.ManifestStateClass, error) {
	var session string
	var expires time.Time
	var used sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT session_hash, expires_at, used_at FROM github_app_manifest_states WHERE state_hash = $1`, stateHash).
		Scan(&session, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.ManifestStateMissing, nil
	}
	if err != nil {
		return "", mapDBError(err)
	}
	return persistence.ClassifyManifestRow(true, session, sessionHash, used.Valid, expires, now), nil
}

// ConsumeManifestState implements the interface. RETURNING keeps the
// mark and the read one statement.
func (r *GitHubAppManifestStateRepository) ConsumeManifestState(ctx context.Context, stateHash, sessionHash string, now time.Time) (string, bool, error) {
	var apiBase string
	err := r.db.QueryRowContext(ctx, `UPDATE github_app_manifest_states SET used_at = $3
		WHERE state_hash = $1 AND session_hash = $2 AND used_at IS NULL AND expires_at > $3
		RETURNING api_base`,
		stateHash, sessionHash, now).Scan(&apiBase)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapDBError(err)
	}
	return apiBase, true, nil
}

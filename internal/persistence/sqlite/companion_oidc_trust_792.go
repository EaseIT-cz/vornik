package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CompanionOIDCTrustRepository is the SQLite driver of
// persistence.CompanionOIDCTrustRepository (GitHub #792, design 27). SQLite
// runs with foreign_keys off, so every read joins api_keys: a trust whose key
// row is gone never matches.
type CompanionOIDCTrustRepository struct{ db *sql.DB }

// NewCompanionOIDCTrustRepository wires the repository.
func NewCompanionOIDCTrustRepository(db *sql.DB) *CompanionOIDCTrustRepository {
	return &CompanionOIDCTrustRepository{db: db}
}

const oidcTrustCols = `t.id, t.key_id, k.project_id, t.issuer, t.audience, t.repository_id, t.repository,
	t.ref, t.job_workflow_ref, t.event_names, t.created_at, t.created_by, t.last_used_at`

// CreateOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) CreateOIDCTrust(ctx context.Context, t persistence.CompanionOIDCTrust) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO companion_key_oidc_trusts
		(id, key_id, issuer, audience, repository_id, repository, ref, job_workflow_ref, event_names, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.KeyID, t.Issuer, t.Audience, t.RepositoryID, t.Repository, t.Ref, t.JobWorkflowRef,
		persistence.EncodeOIDCEventNames(t.EventNames), sqliteTime(t.CreatedAt), t.CreatedBy)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: companion_key_oidc_trusts.") {
		return persistence.ErrOIDCTrustDuplicate
	}
	return err
}

// OIDCTrustsForRepository implements the interface.
func (r *CompanionOIDCTrustRepository) OIDCTrustsForRepository(ctx context.Context, issuer string, repositoryID int64) ([]persistence.CompanionOIDCTrust, error) {
	return r.query(ctx, `SELECT `+oidcTrustCols+` FROM companion_key_oidc_trusts t JOIN api_keys k ON k.id = t.key_id
		WHERE t.issuer = ? AND t.repository_id = ? ORDER BY t.created_at, t.id`, issuer, repositoryID)
}

// OIDCTrustsForProject implements the interface.
func (r *CompanionOIDCTrustRepository) OIDCTrustsForProject(ctx context.Context, projectID string) ([]persistence.CompanionOIDCTrust, error) {
	return r.query(ctx, `SELECT `+oidcTrustCols+` FROM companion_key_oidc_trusts t JOIN api_keys k ON k.id = t.key_id
		WHERE k.project_id = ? ORDER BY t.created_at, t.id`, projectID)
}

// DeleteOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) DeleteOIDCTrust(ctx context.Context, projectID, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM companion_key_oidc_trusts WHERE id = ?
		AND key_id IN (SELECT id FROM api_keys WHERE project_id = ?)`, id, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TouchOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) TouchOIDCTrust(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE companion_key_oidc_trusts SET last_used_at = ? WHERE id = ?`, sqliteTime(at), id)
	return err
}

func (r *CompanionOIDCTrustRepository) query(ctx context.Context, q string, args ...any) ([]persistence.CompanionOIDCTrust, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.CompanionOIDCTrust
	for rows.Next() {
		var t persistence.CompanionOIDCTrust
		var events, created string
		var used sql.NullString
		if err := rows.Scan(&t.ID, &t.KeyID, &t.ProjectID, &t.Issuer, &t.Audience, &t.RepositoryID, &t.Repository,
			&t.Ref, &t.JobWorkflowRef, &events, &created, &t.CreatedBy, &used); err != nil {
			return nil, err
		}
		t.EventNames = persistence.DecodeOIDCEventNames(events)
		if t.CreatedAt, err = time.Parse(persistence.SQLiteTimeLayout, created); err != nil {
			return nil, err
		}
		if used.Valid {
			u, err := time.Parse(persistence.SQLiteTimeLayout, used.String)
			if err != nil {
				return nil, err
			}
			t.LastUsedAt = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

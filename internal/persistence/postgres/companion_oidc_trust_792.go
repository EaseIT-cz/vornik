package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// CompanionOIDCTrustRepository is the Postgres driver of
// persistence.CompanionOIDCTrustRepository (GitHub #792, design 27). The
// table cascades from api_keys; reads still join it, as SQLite's must, so a
// trust always lists with its key's project.
type CompanionOIDCTrustRepository struct{ db persistence.DBTX }

// NewCompanionOIDCTrustRepository wires the repository.
func NewCompanionOIDCTrustRepository(db persistence.DBTX) *CompanionOIDCTrustRepository {
	return &CompanionOIDCTrustRepository{db: db}
}

const oidcTrustCols = `t.id, t.key_id, k.project_id, t.issuer, t.audience, t.repository_id, t.repository,
	t.ref, t.job_workflow_ref, t.event_names, t.created_at, t.created_by, t.last_used_at`

// CreateOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) CreateOIDCTrust(ctx context.Context, t persistence.CompanionOIDCTrust) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO companion_key_oidc_trusts
		(id, key_id, issuer, audience, repository_id, repository, ref, job_workflow_ref, event_names, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		t.ID, t.KeyID, t.Issuer, t.Audience, t.RepositoryID, t.Repository, t.Ref, t.JobWorkflowRef,
		persistence.EncodeOIDCEventNames(t.EventNames), t.CreatedAt.UTC(), t.CreatedBy)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "uq_companion_key_oidc_trusts_claims" {
		return persistence.ErrOIDCTrustDuplicate
	}
	return err
}

// OIDCTrustsForRepository implements the interface.
func (r *CompanionOIDCTrustRepository) OIDCTrustsForRepository(ctx context.Context, issuer string, repositoryID int64) ([]persistence.CompanionOIDCTrust, error) {
	return r.query(ctx, `SELECT `+oidcTrustCols+` FROM companion_key_oidc_trusts t JOIN api_keys k ON k.id = t.key_id
		WHERE t.issuer = $1 AND t.repository_id = $2 ORDER BY t.created_at, t.id`, issuer, repositoryID)
}

// OIDCTrustsForProject implements the interface.
func (r *CompanionOIDCTrustRepository) OIDCTrustsForProject(ctx context.Context, projectID string) ([]persistence.CompanionOIDCTrust, error) {
	return r.query(ctx, `SELECT `+oidcTrustCols+` FROM companion_key_oidc_trusts t JOIN api_keys k ON k.id = t.key_id
		WHERE k.project_id = $1 ORDER BY t.created_at, t.id`, projectID)
}

// DeleteOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) DeleteOIDCTrust(ctx context.Context, projectID, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM companion_key_oidc_trusts WHERE id = $1
		AND key_id IN (SELECT id FROM api_keys WHERE project_id = $2)`, id, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TouchOIDCTrust implements the interface.
func (r *CompanionOIDCTrustRepository) TouchOIDCTrust(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE companion_key_oidc_trusts SET last_used_at = $1 WHERE id = $2`, at.UTC(), id)
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
		var events string
		var used sql.NullTime
		if err := rows.Scan(&t.ID, &t.KeyID, &t.ProjectID, &t.Issuer, &t.Audience, &t.RepositoryID, &t.Repository,
			&t.Ref, &t.JobWorkflowRef, &events, &t.CreatedAt, &t.CreatedBy, &used); err != nil {
			return nil, err
		}
		t.EventNames = persistence.DecodeOIDCEventNames(events)
		t.CreatedAt = t.CreatedAt.UTC()
		if used.Valid {
			u := used.Time.UTC()
			t.LastUsedAt = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

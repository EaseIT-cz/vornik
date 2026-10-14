package subjectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// GDPR increment 6 (GitHub #343): the Art 18 restriction record. The store only
// records; enforcement is the live predicate in persistence.ChunkNotRestrictedSQL.
//
// see LLD § https://docs.vornik.io §4.16.1

// CreateRestriction records a restriction. A second ACTIVE restriction for the
// same subject violates idx_ds_restrictions_one_active and is reported as
// datasubject.ErrRestrictionActive. An unknown subject, or a request that does
// not exist or belongs to another subject, is persistence.ErrNotFound: the
// restriction must resolve to the request that asked for it (design §4.19.3).
func (r *Repository) CreateRestriction(ctx context.Context, res datasubject.Restriction) error {
	if !res.Active() {
		return errors.New("subjectstore: CreateRestriction records an active restriction; lift it with SaveRestrictionLift")
	}
	return r.withSubject(ctx, res.SubjectID, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM data_subject_requests WHERE id = $1 AND subject_id = $2`,
			res.RequestID, res.SubjectID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("subjectstore: request %q for subject %q: %w",
				res.RequestID, res.SubjectID, persistence.ErrNotFound)
		}
		if err != nil {
			return r.d.mapErr(err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO data_subject_restrictions
			       (id, subject_id, request_id, ground, restricted_by, restricted_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			res.ID, res.SubjectID, res.RequestID, string(res.Ground), res.RestrictedBy,
			r.d.bindTime(res.RestrictedAt))
		if err = r.d.mapErr(err); errors.Is(err, persistence.ErrDuplicateKey) {
			return datasubject.ErrRestrictionActive
		}
		return err
	})
}

// ActiveRestriction returns the subject's restriction in force, or
// datasubject.ErrNoActiveRestriction.
func (r *Repository) ActiveRestriction(ctx context.Context, subjectID string) (datasubject.Restriction, error) {
	var (
		res    datasubject.Restriction
		ground string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, subject_id, request_id, ground, restricted_by, restricted_at
		  FROM data_subject_restrictions
		 WHERE subject_id = $1 AND lifted_at IS NULL`, subjectID,
	).Scan(&res.ID, &res.SubjectID, &res.RequestID, &ground, &res.RestrictedBy, r.d.scanTime(&res.RestrictedAt))
	if errors.Is(err, sql.ErrNoRows) {
		return datasubject.Restriction{}, datasubject.ErrNoActiveRestriction
	}
	if err != nil {
		return datasubject.Restriction{}, fmt.Errorf("subjectstore: read active restriction for %s: %w", subjectID, err)
	}
	res.Ground = datasubject.RestrictionGround(ground)
	return res, nil
}

// SaveRestrictionLift records a lift on a still-active restriction. The row is
// UPDATED, never deleted — a lifted restriction is accountability evidence — and
// the guard on lifted_at IS NULL refuses to overwrite an earlier lift.
func (r *Repository) SaveRestrictionLift(ctx context.Context, res datasubject.Restriction) error {
	if res.Active() {
		return errors.New("subjectstore: SaveRestrictionLift needs a lifted restriction (see Restriction.Lift)")
	}
	out, err := r.db.ExecContext(ctx, `
		UPDATE data_subject_restrictions
		   SET lifted_at = $2, lifted_by = $3, lift_reason = $4,
		       lift_request_id = $5, subject_informed_at = $6
		 WHERE id = $1 AND lifted_at IS NULL`,
		res.ID, r.d.bindTime(res.LiftedAt), res.LiftedBy, res.LiftReason,
		nullIfEmpty(res.LiftRequestID), r.d.bindTimeOrNull(res.SubjectInformedAt))
	if err != nil {
		return fmt.Errorf("subjectstore: record lift of %s: %w", res.ID, err)
	}
	n, err := out.RowsAffected()
	if err != nil {
		return fmt.Errorf("subjectstore: rows affected lifting %s: %w", res.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("subjectstore: restriction %s is not active (already lifted, or unknown)", res.ID)
	}
	return nil
}

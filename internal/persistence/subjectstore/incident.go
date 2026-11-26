package subjectstore

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/incident"
)

// IncidentRepository persists the Art 33/34 breach ledger on either driver.
//
// It was postgres.IncidentRepository until GitHub #475 (2026-11-23), when the
// SQLite schema gained security_incidents. It shares the subject store's
// Dialect, so a timestamp binds in the driver's one form and errors map to the
// same persistence sentinels. The statements bind the clock rather than call
// NOW(): SQLite has no NOW(), and its table has no timestamp defaults.
//
// see LLD § https://docs.vornik.io §4.21
type IncidentRepository struct {
	db  *sql.DB
	d   Dialect
	now func() time.Time
}

// NewIncidentRepository constructs the repository over db with the driver's
// dialect.
func NewIncidentRepository(db *sql.DB, d Dialect) *IncidentRepository {
	return &IncidentRepository{db: db, d: d, now: func() time.Time { return time.Now().UTC() }}
}

// incidentColumns is the full column set Get and List read.
const incidentColumns = `id, state, occurred_at, became_aware_at, facts, effects, remedial,
		        authority_risk, authority_risk_reason, notified_authority_at, authority_reference,
		        subject_risk, subject_risk_reason, notified_subjects_at, subject_exemption,
		        assessed_by, closed_at`

// Create records a newly detected incident and starts the 72-hour clock.
func (r *IncidentRepository) Create(ctx context.Context, i incident.Incident) error {
	now := r.d.bindTime(r.now())
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO security_incidents (id, state, occurred_at, became_aware_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		i.ID, string(i.State), r.d.bindTimeOrNull(i.OccurredAt), r.d.bindTime(i.BecameAwareAt), now, now)
	return r.d.mapErr(err)
}

// Get fetches one incident.
func (r *IncidentRepository) Get(ctx context.Context, id string) (incident.Incident, error) {
	return r.scan(r.db.QueryRowContext(ctx,
		`SELECT `+incidentColumns+` FROM security_incidents WHERE id = $1`, id).Scan)
}

// List returns incidents in EVERY state, newest awareness first, id descending
// as the tiebreaker so offset paging is deterministic (regulatory record design
// §6). ListLive is the deadline query; this is the record.
func (r *IncidentRepository) List(ctx context.Context, limit, offset int) ([]incident.Incident, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+incidentColumns+` FROM security_incidents
		  ORDER BY became_aware_at DESC, id DESC
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []incident.Incident
	for rows.Next() {
		i, err := r.scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// scan reads incidentColumns.
func (r *IncidentRepository) scan(scan func(...any) error) (incident.Incident, error) {
	var i incident.Incident
	var state string
	if err := scan(&i.ID, &state, r.d.scanTime(&i.OccurredAt), r.d.scanTime(&i.BecameAwareAt),
		&i.Facts, &i.Effects, &i.Remedial,
		&i.AuthorityRisk, &i.AuthorityRiskReason, r.d.scanTime(&i.NotifiedAuthorityAt), &i.AuthorityReference,
		&i.SubjectRisk, &i.SubjectRiskReason, r.d.scanTime(&i.NotifiedSubjectsAt), &i.SubjectExemption,
		&i.AssessedBy, r.d.scanTime(&i.ClosedAt)); err != nil {
		return incident.Incident{}, r.d.mapErr(err)
	}
	i.State = incident.State(state)
	return i, nil
}

// Save persists a state transition.
func (r *IncidentRepository) Save(ctx context.Context, i incident.Incident) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE security_incidents
		    SET state = $2, facts = $3, effects = $4, remedial = $5,
		        authority_risk = $6, authority_risk_reason = $7,
		        notified_authority_at = $8, authority_reference = $9,
		        subject_risk = $10, subject_risk_reason = $11,
		        notified_subjects_at = $12, subject_exemption = $13,
		        assessed_by = $14, closed_at = $15, updated_at = $16
		  WHERE id = $1`,
		i.ID, string(i.State), i.Facts, i.Effects, i.Remedial,
		i.AuthorityRisk, i.AuthorityRiskReason,
		r.d.bindTimeOrNull(i.NotifiedAuthorityAt), i.AuthorityReference,
		i.SubjectRisk, i.SubjectRiskReason,
		r.d.bindTimeOrNull(i.NotifiedSubjectsAt), i.SubjectExemption,
		i.AssessedBy, r.d.bindTimeOrNull(i.ClosedAt), r.d.bindTime(r.now()))
	return r.d.mapErr(err)
}

// ListLive returns incidents whose Art 33 obligation is undischarged, oldest
// awareness first — the query the 72-hour deadline check runs.
func (r *IncidentRepository) ListLive(ctx context.Context) ([]incident.Incident, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, state, became_aware_at, authority_risk, subject_risk
		   FROM security_incidents
		  WHERE state IN ('detected', 'assessed')
		  ORDER BY became_aware_at`)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []incident.Incident
	for rows.Next() {
		var i incident.Incident
		var state string
		if err := rows.Scan(&i.ID, &state, r.d.scanTime(&i.BecameAwareAt), &i.AuthorityRisk, &i.SubjectRisk); err != nil {
			return nil, r.d.mapErr(err)
		}
		i.State = incident.State(state)
		out = append(out, i)
	}
	return out, rows.Err()
}

// CountLiveByDeadline reports how many undischarged incidents are overdue and
// how many are approaching the 72-hour limit. Used by the doctor check, which
// must not need the whole ledger to answer "is anything on fire".
func (r *IncidentRepository) CountLiveByDeadline(ctx context.Context, now time.Time) (overdue, soon int, err error) {
	live, err := r.ListLive(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, i := range live {
		switch {
		case i.Overdue(now):
			overdue++
		case i.NeedsAttention(now):
			soon++
		}
	}
	return overdue, soon, nil
}

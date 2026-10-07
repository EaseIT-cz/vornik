package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BrokerGrantRepository implements persistence.BrokerGrantRepository over
// SQLite (broker write-actions design, tier 2). It mirrors the Postgres
// implementation; repotest.RunBrokerGrantSuite keeps the two honest.
//
// Serialisation: the daemon's DSN begins read-write transactions IMMEDIATE
// (_txlock=immediate), so a seed approval's count and insert, and a covered
// approval's two statements, run under SQLite's single writer lock. Each
// transaction's first statement is a write as well, which takes the lock
// even on a handle opened without the DSN parameter.
type BrokerGrantRepository struct {
	db *sql.DB
	// hook is the deterministic race seam (tests only).
	hook func(stage string)
}

// NewBrokerGrantRepository wires the repository.
func NewBrokerGrantRepository(db *sql.DB) *BrokerGrantRepository {
	return &BrokerGrantRepository{db: db}
}

var _ persistence.BrokerGrantRepository = (*BrokerGrantRepository)(nil)

// SetGrantHookForTest installs fn, run inside the seed transaction once the
// live grants are counted ("seed-counted") and inside the covered approval
// once the action's transition affected its row ("action-approved").
func (r *BrokerGrantRepository) SetGrantHookForTest(fn func(stage string)) { r.hook = fn }

func (r *BrokerGrantRepository) at(stage string) {
	if r.hook != nil {
		r.hook(stage)
	}
}

const brokerGrantColumns = `id, project_id, namespace, workflow_id, action, key_paths, key_values, key_hash,
	max_uses, uses_left, expires_at, created_at, created_by, seed_action_id, reach_hash_at_creation,
	active, paused, suspended_at, revoked_at, digest_through`

// ApproveSeedAndCreate implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) ApproveSeedAndCreate(ctx context.Context, actionID, shownArgsSHA256, approver string, g *persistence.BrokerStandingGrant, maxLive int, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The seed's own guarded pending→approved, first (it takes the lock).
	if err := transitionErr(tx.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'approved', approver = ?, decided_at = ?
		WHERE action_id = ? AND status = 'pending' AND args_sha256 = ? AND expires_at > ?`,
		approver, sqliteTime(now), actionID, shownArgsSHA256, sqliteTime(now))); err != nil {
		return err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM broker_standing_grants
		WHERE project_id = ? AND active = 1 AND uses_left > 0 AND expires_at > ?`,
		g.ProjectID, sqliteTime(now)).Scan(&live); err != nil {
		return err
	}
	r.at("seed-counted")
	if live >= maxLive {
		return persistence.ErrBrokerGrantLimit
	}
	paths, err := json.Marshal(g.KeyPaths)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO broker_standing_grants (`+brokerGrantColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, NULL, NULL, ?)`,
		g.ID, g.ProjectID, g.Namespace, g.WorkflowID, g.Action, string(paths), g.KeyValuesSealed, g.KeyHash,
		g.MaxUses, g.UsesLeft, sqliteTime(g.ExpiresAt), sqliteTime(g.CreatedAt), g.CreatedBy, g.SeedActionID,
		g.ReachHashAtCreation, sqliteTime(g.DigestThrough)); err != nil {
		return err
	}
	return tx.Commit()
}

// ApproveUnderGrant implements persistence.BrokerGrantRepository. decided_at
// is stamped truncated to the microsecond, as on Postgres (review f819), so
// both drivers agree on which digest window holds an approval.
func (r *BrokerGrantRepository) ApproveUnderGrant(ctx context.Context, actionID, argsSHA256, grantID, keyHash string, now time.Time) error {
	now = now.Truncate(time.Microsecond)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := transitionErr(tx.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'approved', approver = ?, decided_at = ?
		WHERE action_id = ? AND status = 'pending' AND args_sha256 = ? AND expires_at > ?`,
		persistence.BrokerGrantApprover(grantID), sqliteTime(now), actionID, argsSHA256, sqliteTime(now))); err != nil {
		return err
	}
	r.at("action-approved")
	// The guarded decrement: the only path to an approval under a grant.
	n, err := rowsAffected(tx.ExecContext(ctx, `
		UPDATE broker_standing_grants SET uses_left = uses_left - 1
		WHERE id = ? AND key_hash = ? AND active = 1 AND paused = 0 AND suspended_at IS NULL
		  AND uses_left > 0 AND expires_at > ?
		  AND EXISTS (SELECT 1 FROM broker_actions a WHERE a.action_id = ?
		      AND a.project_id = broker_standing_grants.project_id
		      AND a.workflow_id = broker_standing_grants.workflow_id
		      AND a.action_kind = broker_standing_grants.action)`,
		grantID, keyHash, sqliteTime(now), actionID))
	if err != nil {
		return err
	}
	if n != 1 {
		return persistence.ErrBrokerGrantNotCovered
	}
	return tx.Commit()
}

// Get implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Get(ctx context.Context, id string) (*persistence.BrokerStandingGrant, error) {
	g, err := scanBrokerGrant(r.db.QueryRowContext(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return g, err
}

// ListForAction implements persistence.BrokerGrantRepository: every live
// grant first (uncapped), then the 200 most recent non-live ones, appended
// in that order (GitHub #78).
func (r *BrokerGrantRepository) ListForAction(ctx context.Context, projectID, workflowID, action string, now time.Time) ([]*persistence.BrokerStandingGrant, error) {
	live, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE project_id = ? AND workflow_id = ? AND action = ?
		  AND active = 1 AND uses_left > 0 AND expires_at > ? ORDER BY expires_at, id`,
		projectID, workflowID, action, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	dead, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE project_id = ? AND workflow_id = ? AND action = ?
		  AND NOT (active = 1 AND uses_left > 0 AND expires_at > ?)
		ORDER BY expires_at DESC, id LIMIT 200`,
		projectID, workflowID, action, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	return append(live, dead...), nil
}

// List implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) List(ctx context.Context, f persistence.BrokerGrantFilter) ([]*persistence.BrokerStandingGrant, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + brokerGrantColumns + ` FROM broker_standing_grants WHERE namespace = ?`
	args := []any{f.Namespace}
	if f.Namespace == persistence.BrokerGrantAllAgentNamespaces {
		q, args = `SELECT `+brokerGrantColumns+` FROM broker_standing_grants WHERE namespace <> ''`, nil
	}
	if f.Namespace == "" && len(f.ProjectIDs) > 0 {
		q += ` AND project_id IN (?` + strings.Repeat(", ?", len(f.ProjectIDs)-1) + `)`
		for _, p := range f.ProjectIDs {
			args = append(args, p)
		}
	}
	q += ` ORDER BY created_at DESC, id LIMIT ?`
	args = append(args, limit)
	return r.list(ctx, q, args...)
}

// SetPaused implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) SetPaused(ctx context.Context, id string, paused bool) error {
	return grantTransitionErr(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET paused = ? WHERE id = ? AND active = 1`, boolInt(paused), id))
}

// Revoke implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Revoke(ctx context.Context, id string, now time.Time) error {
	return grantTransitionErr(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET active = 0, revoked_at = ? WHERE id = ? AND active = 1`, sqliteTime(now), id))
}

// Suspend implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Suspend(ctx context.Context, id string, now time.Time) (bool, error) {
	n, err := rowsAffected(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET suspended_at = ? WHERE id = ? AND active = 1 AND suspended_at IS NULL`, sqliteTime(now), id))
	return n == 1, err
}

// Confirm implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Confirm(ctx context.Context, id, reachHash string) error {
	return grantTransitionErr(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET suspended_at = NULL, reach_hash_at_creation = ?
		 WHERE id = ? AND active = 1 AND suspended_at IS NOT NULL`, reachHash, id))
}

// CountLive implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) CountLive(ctx context.Context, now time.Time) ([]persistence.BrokerGrantCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT project_id, COUNT(*), COALESCE(SUM(CASE WHEN paused = 1 THEN 1 ELSE 0 END), 0)
		FROM broker_standing_grants WHERE active = 1 AND uses_left > 0 AND expires_at > ?
		GROUP BY project_id`, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.BrokerGrantCount
	for rows.Next() {
		var c persistence.BrokerGrantCount
		if err := rows.Scan(&c.ProjectID, &c.Live, &c.Paused); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CoveredActions implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) CoveredActions(ctx context.Context, grantID string, limit int) ([]*persistence.BrokerAction, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions
		WHERE approver = ? ORDER BY decided_at DESC LIMIT ?`, persistence.BrokerGrantApprover(grantID), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.BrokerAction
	for rows.Next() {
		a, err := scanBrokerAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DigestDue implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) DigestDue(ctx context.Context, before, now time.Time) ([]*persistence.BrokerStandingGrant, error) {
	return r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants g
		WHERE digest_through <= ?
		  AND ((active = 1 AND uses_left > 0 AND expires_at > ?)
		       OR EXISTS (SELECT 1 FROM broker_actions a WHERE a.approver = ? || g.id AND a.decided_at > g.digest_through))
		ORDER BY digest_through LIMIT 500`, sqliteTime(before), sqliteTime(now), persistence.BrokerGrantApproverPrefix)
}

// AdvanceDigest implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) AdvanceDigest(ctx context.Context, id string, from, to time.Time) (int, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := rowsAffected(tx.ExecContext(ctx,
		`UPDATE broker_standing_grants SET digest_through = ? WHERE id = ? AND digest_through = ?`,
		sqliteTime(to), id, sqliteTime(from)))
	if err != nil || n != 1 {
		return 0, false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM broker_actions
		WHERE approver = ? AND decided_at > ? AND decided_at <= ?`,
		persistence.BrokerGrantApprover(id), sqliteTime(from), sqliteTime(to)).Scan(&count); err != nil {
		return 0, false, err
	}
	return count, true, tx.Commit()
}

func (r *BrokerGrantRepository) list(ctx context.Context, q string, args ...any) ([]*persistence.BrokerStandingGrant, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.BrokerStandingGrant
	for rows.Next() {
		g, err := scanBrokerGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanBrokerGrant(s interface{ Scan(dest ...any) error }) (*persistence.BrokerStandingGrant, error) {
	var (
		g                        persistence.BrokerStandingGrant
		paths                    string
		active, paused           int
		expires, created, digest sqlTime
		suspended, revoked       sqlNullTime
	)
	if err := s.Scan(&g.ID, &g.ProjectID, &g.Namespace, &g.WorkflowID, &g.Action, &paths, &g.KeyValuesSealed, &g.KeyHash,
		&g.MaxUses, &g.UsesLeft, &expires, &created, &g.CreatedBy, &g.SeedActionID, &g.ReachHashAtCreation,
		&active, &paused, &suspended, &revoked, &digest); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(paths), &g.KeyPaths); err != nil {
		return nil, err
	}
	g.Active, g.Paused = active == 1, paused == 1
	g.ExpiresAt, g.CreatedAt, g.DigestThrough = expires.Time, created.Time, digest.Time
	if suspended.Valid {
		t := suspended.Time
		g.SuspendedAt = &t
	}
	if revoked.Valid {
		t := revoked.Time
		g.RevokedAt = &t
	}
	return &g, nil
}

func grantTransitionErr(res sql.Result, err error) error {
	n, err := rowsAffected(res, err)
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrBrokerGrantNoTransition
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

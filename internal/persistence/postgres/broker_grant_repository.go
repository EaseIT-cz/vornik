package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

// BrokerGrantRepository implements persistence.BrokerGrantRepository over
// Postgres (broker write-actions design, tier 2). It mirrors the SQLite
// implementation; repotest.RunBrokerGrantSuite keeps the two honest.
//
// Serialisation: a seed approval takes
// pg_advisory_xact_lock(hashtext('broker_grants:' || project_id)) before it
// counts the project's live grants (the single-int4 form; review 61a5), so
// the count and the insert are serialised for exactly the set the bound
// counts. A covered approval's decrement is one conditional UPDATE: its row
// lock serialises concurrent decrements, and READ COMMITTED re-evaluates the
// WHERE clause against the committed row, so the last use goes once.
type BrokerGrantRepository struct {
	db   persistence.DBTX
	hook func(stage string)
}

// NewBrokerGrantRepository wires the repository.
func NewBrokerGrantRepository(db persistence.DBTX) *BrokerGrantRepository {
	return &BrokerGrantRepository{db: db}
}

var _ persistence.BrokerGrantRepository = (*BrokerGrantRepository)(nil)

// SetGrantHookForTest installs the deterministic race seam (tests only):
// "seed-counted" after the live count, "action-approved" after the covered
// action's own transition.
func (r *BrokerGrantRepository) SetGrantHookForTest(fn func(stage string)) { r.hook = fn }

func (r *BrokerGrantRepository) at(stage string) {
	if r.hook != nil {
		r.hook(stage)
	}
}

const brokerGrantColumns = `id, project_id, namespace, workflow_id, action, key_paths, key_values, key_hash,
	max_uses, uses_left, expires_at, created_at, created_by, seed_action_id, reach_hash_at_creation,
	active, paused, suspended_at, revoked_at, digest_through, account_label, account_identity`

func (r *BrokerGrantRepository) withTx(ctx context.Context, fn func(x persistence.DBTX) error) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		return fn(r.db)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	committed = true
	return mapDBError(tx.Commit())
}

// ApproveSeedAndCreate implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) ApproveSeedAndCreate(ctx context.Context, actionID, shownArgsSHA256, approver string, g *persistence.BrokerStandingGrant, maxLive int, now time.Time) error {
	paths, err := json.Marshal(g.KeyPaths)
	if err != nil {
		return err
	}
	return r.withTx(ctx, func(x persistence.DBTX) error {
		a, err := guardedPendingAction(ctx, x, actionID)
		if err != nil {
			return err
		}
		if ns, agent := agentns.FromID(a.ProjectID); agent && (g.ProjectID != a.ProjectID || g.Namespace != ns) {
			return persistence.ErrAccountTransition
		}

		if err := checkAccountGuard(ctx, x, g.Namespace, g.DecisionGuard); err != nil {
			return err
		}
		if _, err := x.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('broker_grants:' || $1::text))`, g.ProjectID); err != nil {
			return mapDBError(err)
		}
		if err := approveGuardedAction(ctx, x, a, shownArgsSHA256, approver, now); err != nil {
			return err
		}
		var live int
		if err := x.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM broker_standing_grants
			WHERE project_id = $1 AND active AND uses_left > 0 AND expires_at > $2`,
			g.ProjectID, now).Scan(&live); err != nil {
			return mapDBError(err)
		}
		r.at("seed-counted")
		if live >= maxLive {
			return persistence.ErrBrokerGrantLimit
		}
		_, err = x.ExecContext(ctx, `INSERT INTO broker_standing_grants (`+brokerGrantColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, TRUE, FALSE, NULL, NULL, $16, $17, $18)`,
			g.ID, g.ProjectID, g.Namespace, g.WorkflowID, g.Action, string(paths), g.KeyValuesSealed, g.KeyHash,
			g.MaxUses, g.UsesLeft, g.ExpiresAt, g.CreatedAt, g.CreatedBy, g.SeedActionID,
			g.ReachHashAtCreation, g.DigestThrough.Truncate(time.Microsecond), g.AccountLabel, g.AccountIdentity)
		return mapDBError(err)
	})
}

// ApproveUnderGrant implements persistence.BrokerGrantRepository.
// decided_at is stamped truncated to the microsecond: Postgres would round a
// finer instant, possibly past the digest window AdvanceDigest bounds with
// truncated instants (review f819).
func (r *BrokerGrantRepository) ApproveUnderGrant(ctx context.Context, actionID, argsSHA256, grantID, keyHash string, now time.Time) error {
	now = now.Truncate(time.Microsecond)
	return r.withTx(ctx, func(x persistence.DBTX) error {
		a, err := guardedPendingAction(ctx, x, actionID)
		if err != nil {
			return err
		}

		if err := approveGuardedAction(ctx, x, a, argsSHA256, persistence.BrokerGrantApprover(grantID), now); err != nil {
			return err
		}
		r.at("action-approved")
		n, err := brokerRowsAffected(x.ExecContext(ctx, `
			UPDATE broker_standing_grants g SET uses_left = g.uses_left - 1
			WHERE g.id = $1 AND g.key_hash = $2 AND g.active AND NOT g.paused AND g.suspended_at IS NULL
			  AND g.uses_left > 0 AND g.expires_at > $3
			  AND EXISTS (SELECT 1 FROM broker_actions a WHERE a.action_id = $4
			      AND a.project_id = g.project_id AND a.workflow_id = g.workflow_id AND a.action_kind = g.action)`,
			grantID, keyHash, now, actionID))
		if err != nil {
			return mapDBError(err)
		}
		if n != 1 {
			return persistence.ErrBrokerGrantNotCovered
		}
		return nil
	})
}

// Get implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Get(ctx context.Context, id string) (*persistence.BrokerStandingGrant, error) {
	g, err := scanBrokerGrant(r.db.QueryRowContext(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return g, mapDBError(err)
}

// ListForAction implements persistence.BrokerGrantRepository: every live
// grant first (uncapped), then the 200 most recent non-live ones, appended
// in that order (GitHub #78).
func (r *BrokerGrantRepository) ListForAction(ctx context.Context, projectID, workflowID, action string, now time.Time) ([]*persistence.BrokerStandingGrant, error) {
	live, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE project_id = $1 AND workflow_id = $2 AND action = $3
		  AND active AND uses_left > 0 AND expires_at > $4 ORDER BY expires_at, id`,
		projectID, workflowID, action, now)
	if err != nil {
		return nil, err
	}
	dead, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE project_id = $1 AND workflow_id = $2 AND action = $3
		  AND NOT (active AND uses_left > 0 AND expires_at > $4)
		ORDER BY expires_at DESC, id LIMIT 200`,
		projectID, workflowID, action, now)
	if err != nil {
		return nil, err
	}
	return append(live, dead...), nil
}

// List implements persistence.BrokerGrantRepository: for the filter's scope,
// the live grants first (newest first, at most f.LiveCap, default
// BrokerGrantLiveListCap; over it the first LiveCap come back with
// ErrBrokerGrantLiveTruncated), then the f.Limit most recent non-live
// ones, appended in that order and never re-sorted (GitHub #93: one query
// ordered by created_at with LIMIT 200 hid an old live grant behind newer
// dead ones).
func (r *BrokerGrantRepository) List(ctx context.Context, f persistence.BrokerGrantFilter) ([]*persistence.BrokerStandingGrant, error) {
	if f.Now.IsZero() {
		return nil, persistence.ErrBrokerGrantFilterNoNow
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	// The scope binds its own leading placeholders; now and the dead tail's
	// cap follow them.
	var scope string
	var scopeArgs []any
	switch {
	case f.Namespace == "" && len(f.ProjectIDs) > 0:
		scope, scopeArgs = `namespace = '' AND project_id = ANY($1)`, []any{pq.Array(f.ProjectIDs)}
	case f.Namespace == persistence.BrokerGrantAllAgentNamespaces:
		scope = `namespace <> ''`
	default:
		scope, scopeArgs = `namespace = $1`, []any{f.Namespace}
	}
	n := len(scopeArgs)
	live := fmt.Sprintf(`active AND uses_left > 0 AND expires_at > $%d`, n+1)
	liveCap := f.LiveCap
	if liveCap <= 0 {
		liveCap = persistence.BrokerGrantLiveListCap
	}
	liveArgs := append(append([]any{}, scopeArgs...), f.Now, liveCap+1)
	liveRows, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE `+scope+` AND `+live+` ORDER BY created_at DESC, id LIMIT `+fmt.Sprintf("$%d", n+2), liveArgs...)
	if err != nil {
		return nil, err
	}
	var truncated error
	if len(liveRows) > liveCap {
		liveRows, truncated = liveRows[:liveCap], persistence.ErrBrokerGrantLiveTruncated
	}
	deadArgs := append(append([]any{}, scopeArgs...), f.Now, limit)
	deadRows, err := r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants
		WHERE `+scope+` AND NOT (`+live+`) ORDER BY created_at DESC, id LIMIT `+fmt.Sprintf("$%d", n+2), deadArgs...)
	if err != nil {
		return nil, err
	}
	return append(liveRows, deadRows...), truncated
}

// SetPaused implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) SetPaused(ctx context.Context, id string, paused bool) error {
	return pgGrantTransitionErr(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET paused = $1 WHERE id = $2 AND active`, paused, id))
}

// Revoke implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Revoke(ctx context.Context, id string, now time.Time) error {
	return pgGrantTransitionErr(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET active = FALSE, revoked_at = $1 WHERE id = $2 AND active`, now, id))
}

// Suspend implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Suspend(ctx context.Context, id string, now time.Time) (bool, error) {
	n, err := brokerRowsAffected(r.db.ExecContext(ctx,
		`UPDATE broker_standing_grants SET suspended_at = $1 WHERE id = $2 AND active AND suspended_at IS NULL`, now, id))
	return n == 1, mapDBError(err)
}

// Confirm implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) Confirm(ctx context.Context, id, reachHash string) error {
	return r.ConfirmGuarded(ctx, id, reachHash, nil)
}

// ConfirmGuarded reaffirms a suspended grant under the supplied account generations.
func (r *BrokerGrantRepository) ConfirmGuarded(ctx context.Context, id, reachHash string, guard persistence.AccountDecisionGuard) error {
	return r.withTx(ctx, func(x persistence.DBTX) error {
		return r.confirmTransaction(ctx, x, id, guard, func(x persistence.DBTX) error {
			return pgGrantTransitionErr(x.ExecContext(ctx,
				`UPDATE broker_standing_grants SET suspended_at = NULL, reach_hash_at_creation = $1
		 WHERE id = $2 AND active AND suspended_at IS NOT NULL`, reachHash, id))
		})
	})
}

// ConfirmAccount implements persistence.BrokerGrantRepository; agent grants require a guard.
func (r *BrokerGrantRepository) ConfirmAccount(ctx context.Context, id, reachHash, oldLabel, oldIdentity, newLabel, newIdentity string) error {
	return r.ConfirmAccountGuarded(ctx, id, reachHash, oldLabel, oldIdentity, newLabel, newIdentity, nil)
}

// ConfirmAccountGuarded updates account metadata and reaffirms under guarded generations.
func (r *BrokerGrantRepository) ConfirmAccountGuarded(ctx context.Context, id, reachHash, oldLabel, oldIdentity, newLabel, newIdentity string, guard persistence.AccountDecisionGuard) error {
	return r.withTx(ctx, func(x persistence.DBTX) error {
		return r.confirmTransaction(ctx, x, id, guard, func(x persistence.DBTX) error {
			return pgGrantTransitionErr(x.ExecContext(ctx,
				`UPDATE broker_standing_grants SET suspended_at = NULL, reach_hash_at_creation = $1, account_label = $2, account_identity = $3
		 WHERE id = $4 AND active AND suspended_at IS NOT NULL AND account_label = $5 AND account_identity = $6`,
				reachHash, newLabel, newIdentity, id, oldLabel, oldIdentity))
		})
	})
}

// confirmTransaction locks epoch rows before the grant UPDATE, in the same transaction.
func (r *BrokerGrantRepository) confirmTransaction(ctx context.Context, x persistence.DBTX, id string, guard persistence.AccountDecisionGuard, update func(persistence.DBTX) error) error {
	var ns string
	if err := x.QueryRowContext(ctx, `SELECT namespace FROM broker_standing_grants WHERE id=$1`, id).Scan(&ns); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrBrokerGrantNoTransition
		}
		return err
	}
	if err := checkAccountGuard(ctx, x, ns, guard); err != nil {
		return err
	}
	return update(x)
}

// CountLive implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) CountLive(ctx context.Context, now time.Time) ([]persistence.BrokerGrantCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT project_id, COUNT(*), COUNT(*) FILTER (WHERE paused)
		FROM broker_standing_grants WHERE active AND uses_left > 0 AND expires_at > $1
		GROUP BY project_id`, now)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.BrokerGrantCount
	for rows.Next() {
		var c persistence.BrokerGrantCount
		if err := rows.Scan(&c.ProjectID, &c.Live, &c.Paused); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, c)
	}
	return out, mapDBError(rows.Err())
}

// CoveredActions implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) CoveredActions(ctx context.Context, grantID string, limit int) ([]*persistence.BrokerAction, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions
		WHERE approver = $1 ORDER BY decided_at DESC LIMIT $2`, persistence.BrokerGrantApprover(grantID), limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.BrokerAction
	for rows.Next() {
		a, err := scanBrokerAction(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, a)
	}
	return out, mapDBError(rows.Err())
}

// DigestDue implements persistence.BrokerGrantRepository.
func (r *BrokerGrantRepository) DigestDue(ctx context.Context, before, now time.Time) ([]*persistence.BrokerStandingGrant, error) {
	return r.list(ctx, `SELECT `+brokerGrantColumns+` FROM broker_standing_grants g
		WHERE digest_through <= $1
		  AND ((active AND uses_left > 0 AND expires_at > $2)
		       OR EXISTS (SELECT 1 FROM broker_actions a WHERE a.approver = $3 || g.id AND a.decided_at > g.digest_through))
		ORDER BY digest_through LIMIT 500`, before, now, persistence.BrokerGrantApproverPrefix)
}

// AdvanceDigest implements persistence.BrokerGrantRepository. Postgres keeps
// microseconds, so both instants are truncated to them: from is a value read
// back, and to is compared by a later pass.
func (r *BrokerGrantRepository) AdvanceDigest(ctx context.Context, id string, from, to time.Time) (int, bool, error) {
	from, to = from.Truncate(time.Microsecond), to.Truncate(time.Microsecond)
	count, ok := 0, false
	err := r.withTx(ctx, func(x persistence.DBTX) error {
		n, err := brokerRowsAffected(x.ExecContext(ctx,
			`UPDATE broker_standing_grants SET digest_through = $1 WHERE id = $2 AND digest_through = $3`, to, id, from))
		if err != nil || n != 1 {
			return mapDBError(err)
		}
		ok = true
		return mapDBError(x.QueryRowContext(ctx, `SELECT COUNT(*) FROM broker_actions
			WHERE approver = $1 AND decided_at > $2 AND decided_at <= $3`,
			persistence.BrokerGrantApprover(id), from, to).Scan(&count))
	})
	if err != nil || !ok {
		return 0, false, err
	}
	return count, true, nil
}

func (r *BrokerGrantRepository) list(ctx context.Context, q string, args ...any) ([]*persistence.BrokerStandingGrant, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.BrokerStandingGrant
	for rows.Next() {
		g, err := scanBrokerGrant(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, g)
	}
	return out, mapDBError(rows.Err())
}

func scanBrokerGrant(s interface{ Scan(dest ...any) error }) (*persistence.BrokerStandingGrant, error) {
	var (
		g                  persistence.BrokerStandingGrant
		paths              string
		suspended, revoked sql.NullTime
	)
	if err := s.Scan(&g.ID, &g.ProjectID, &g.Namespace, &g.WorkflowID, &g.Action, &paths, &g.KeyValuesSealed, &g.KeyHash,
		&g.MaxUses, &g.UsesLeft, &g.ExpiresAt, &g.CreatedAt, &g.CreatedBy, &g.SeedActionID, &g.ReachHashAtCreation,
		&g.Active, &g.Paused, &suspended, &revoked, &g.DigestThrough, &g.AccountLabel, &g.AccountIdentity); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(paths), &g.KeyPaths); err != nil {
		return nil, err
	}
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

func pgGrantTransitionErr(res sql.Result, err error) error {
	n, err := brokerRowsAffected(res, err)
	if err != nil {
		return mapDBError(err)
	}
	if n == 0 {
		return persistence.ErrBrokerGrantNoTransition
	}
	return nil
}

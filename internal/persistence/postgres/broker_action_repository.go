package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BrokerActionRepository implements persistence.BrokerActionRepository over
// Postgres. It mirrors the SQLite implementation statement for statement; the
// shared repotest.RunBrokerActionSuite keeps the two honest.
type BrokerActionRepository struct {
	db persistence.DBTX
}

// NewBrokerActionRepository wires the repository.
func NewBrokerActionRepository(db persistence.DBTX) *BrokerActionRepository {
	return &BrokerActionRepository{db: db}
}

var _ persistence.BrokerActionRepository = (*BrokerActionRepository)(nil)

const brokerActionColumns = `action_id, project_id, task_id, api_key_id, workflow_id, action_kind, tool,
	args_json, args_sha256, status, approver, outcome_json, outcome_class,
	created_at, expires_at, decided_at, executed_at`

// Stage upserts the (task_id, action_kind) row. The ON CONFLICT update is
// guarded by the existing row's status, so a pending or later row keeps its
// arguments: write-once from pending onward (design §5.2).
func (r *BrokerActionRepository) Stage(ctx context.Context, a *persistence.BrokerAction) (bool, error) {
	if !persistence.BrokerActionStageable(a.Status) {
		return false, fmt.Errorf("postgres: broker action Stage: status %q is not stageable", a.Status)
	}
	if a.ActionID == "" {
		a.ActionID = persistence.GenerateID("bact")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO broker_actions (`+brokerActionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, NULL, NULL, $11, $12, NULL, NULL)
		ON CONFLICT (task_id, action_kind) DO UPDATE SET
			args_json = excluded.args_json,
			args_sha256 = excluded.args_sha256,
			status = excluded.status,
			tool = excluded.tool,
			expires_at = excluded.expires_at
		WHERE broker_actions.status IN ('staged', 'proposal_missing', 'proposal_invalid', 'discarded')`,
		a.ActionID, a.ProjectID, a.TaskID, nullIfEmpty(a.APIKeyID), a.WorkflowID, a.ActionKind, a.Tool,
		a.ArgsJSON, a.ArgsSHA256, a.Status,
		a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	// A fresh insert and a permitted replacement both affect one row; the
	// caller cannot tell them apart and does not need to. A refused
	// replacement (row already pending or later) affects none.
	return n == 1, nil
}

// PromoteStaged implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) PromoteStaged(ctx context.Context, taskID string) (int64, error) {
	return brokerRowsAffected(r.db.ExecContext(ctx,
		`UPDATE broker_actions SET status = 'pending' WHERE task_id = $1 AND status = 'staged'`, taskID))
}

// PromoteStagedOfCompletedTasks implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) PromoteStagedOfCompletedTasks(ctx context.Context) (int64, error) {
	return brokerRowsAffected(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'pending'
		WHERE status = 'staged'
		  AND task_id IN (SELECT id FROM tasks WHERE status = 'COMPLETED')`))
}

// DiscardStagedOrphans implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) DiscardStagedOrphans(ctx context.Context) (int64, error) {
	return brokerRowsAffected(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'discarded'
		WHERE status = 'staged'
		  AND task_id IN (SELECT id FROM tasks WHERE status NOT IN ('COMPLETED', 'QUEUED', 'LEASED', 'RUNNING'))`))
}

// Get implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) Get(ctx context.Context, actionID string) (*persistence.BrokerAction, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE action_id = $1`, actionID)
	a, err := scanBrokerAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return a, err
}

// ListByTask implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) ListByTask(ctx context.Context, taskID string) ([]*persistence.BrokerAction, error) {
	return r.list(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE task_id = $1 ORDER BY created_at, action_kind`, taskID)
}

// ListByStatus implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) ListByStatus(ctx context.Context, projectID, status string, limit int) ([]*persistence.BrokerAction, error) {
	if limit <= 0 {
		limit = 100
	}
	if projectID == "" {
		return r.list(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE status = $1 ORDER BY created_at LIMIT $2`, status, limit)
	}
	return r.list(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE status = $1 AND project_id = $2 ORDER BY created_at LIMIT $3`, status, projectID, limit)
}

// Approve implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) Approve(ctx context.Context, actionID, shownArgsSHA256, approver string, now time.Time) error {
	return brokerTransitionErr(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'approved', approver = $1, decided_at = $2
		WHERE action_id = $3 AND status = 'pending' AND args_sha256 = $4 AND expires_at > $5`,
		approver, now, actionID, shownArgsSHA256, now))
}

// Reject implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) Reject(ctx context.Context, actionID, approver string, now time.Time) error {
	return brokerTransitionErr(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'rejected', approver = $1, decided_at = $2
		WHERE action_id = $3 AND status IN ('pending', 'approved')`,
		approver, now, actionID))
}

// ClaimForExecution implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) ClaimForExecution(ctx context.Context, actionID string, now time.Time) (bool, error) {
	n, err := brokerRowsAffected(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'executing', executed_at = $2
		WHERE action_id = $1 AND status = 'approved' AND expires_at > $2`, actionID, now))
	return n == 1, err
}

// Finish implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) Finish(ctx context.Context, actionID, status, outcomeClass string, outcome []byte, now time.Time) error {
	switch status {
	case persistence.BrokerActionExecuted, persistence.BrokerActionFailed, persistence.BrokerActionUnknown:
	default:
		return fmt.Errorf("postgres: broker action Finish: invalid target status %q", status)
	}
	return brokerTransitionErr(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = $1, outcome_class = $2, outcome_json = $3, executed_at = $4
		WHERE action_id = $5 AND status = 'executing'`,
		status, outcomeClass, outcome, now, actionID))
}

// Resolve implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) Resolve(ctx context.Context, actionID, status, approver string, note []byte, now time.Time) error {
	switch status {
	case persistence.BrokerActionExecuted, persistence.BrokerActionFailed:
	default:
		return fmt.Errorf("postgres: broker action Resolve: invalid target status %q", status)
	}
	if len(note) == 0 {
		note = []byte(`{}`)
	}
	return brokerTransitionErr(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = $1, outcome_class = 'operator_resolved', outcome_json = $2, approver = COALESCE(approver, $3), executed_at = $4
		WHERE action_id = $5 AND status IN ('executing', 'unknown')`,
		status, note, approver, now, actionID))
}

// ExpireDue implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	return brokerRowsAffected(r.db.ExecContext(ctx, `
		UPDATE broker_actions SET status = 'expired'
		WHERE status IN ('pending', 'approved') AND expires_at <= $1`, now))
}

// CountStuck implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) CountStuck(ctx context.Context, now time.Time, stuckAfter, approvedAfter time.Duration) ([]persistence.BrokerActionStuck, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT project_id, status, COUNT(*) FROM broker_actions
		WHERE (status IN ('executing', 'unknown') AND COALESCE(executed_at, decided_at, created_at) <= $1)
		   OR (status = 'approved' AND decided_at <= $2)
		GROUP BY project_id, status`,
		now.Add(-stuckAfter), now.Add(-approvedAfter))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.BrokerActionStuck
	for rows.Next() {
		var s persistence.BrokerActionStuck
		if err := rows.Scan(&s.ProjectID, &s.Status, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// list implements persistence.BrokerActionRepository.
func (r *BrokerActionRepository) list(ctx context.Context, query string, args ...any) ([]*persistence.BrokerAction, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
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

func scanBrokerAction(s interface{ Scan(dest ...any) error }) (*persistence.BrokerAction, error) {
	var (
		a                              persistence.BrokerAction
		apiKey, approver, outcomeClass sql.NullString
		decided, executed              sql.NullTime
	)
	if err := s.Scan(&a.ActionID, &a.ProjectID, &a.TaskID, &apiKey, &a.WorkflowID, &a.ActionKind, &a.Tool,
		&a.ArgsJSON, &a.ArgsSHA256, &a.Status, &approver, &a.OutcomeJSON, &outcomeClass,
		&a.CreatedAt, &a.ExpiresAt, &decided, &executed); err != nil {
		return nil, err
	}
	a.APIKeyID, a.Approver, a.OutcomeClass = apiKey.String, approver.String, outcomeClass.String
	if decided.Valid {
		t := decided.Time
		a.DecidedAt = &t
	}
	if executed.Valid {
		t := executed.Time
		a.ExecutedAt = &t
	}
	return &a, nil
}

func brokerRowsAffected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func brokerTransitionErr(res sql.Result, err error) error {
	n, err := brokerRowsAffected(res, err)
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrBrokerActionNoTransition
	}
	return nil
}

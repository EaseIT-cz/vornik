package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// CompanionPushOutbox implements persistence.CompanionPushOutbox — the
// broker write-actions design §7a outbox over a2a_push_configs and
// broker_actions.
type CompanionPushOutbox struct{ db *sql.DB }

// NewCompanionPushOutbox builds the outbox.
func NewCompanionPushOutbox(db *sql.DB) *CompanionPushOutbox { return &CompanionPushOutbox{db: db} }

// actionFrontStateSQL maps a pushable status to its §6 name; other statuses
// are excluded by the WHERE clause.
const actionFrontStateSQL = `CASE a.status WHEN 'pending' THEN 'pending_approval' ELSE a.status END`

// DueTaskPushes implements persistence.CompanionPushOutbox.
func (o *CompanionPushOutbox) DueTaskPushes(ctx context.Context, limit int) ([]persistence.TaskPushDue, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.db.QueryContext(ctx, `
		SELECT c.task_id, t.project_id, t.status, c.url, COALESCE(c.token, ''), c.pushed_state
		FROM a2a_push_configs c JOIN tasks t ON t.id = c.task_id
		WHERE t.creation_source = 'COMPANION'
		  AND t.status IN ('COMPLETED', 'FAILED', 'CANCELLED')
		  AND (c.pushed_state IS NULL OR c.pushed_state <> t.status)
		ORDER BY c.created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.TaskPushDue
	for rows.Next() {
		var d persistence.TaskPushDue
		var pushed sql.NullString
		if err := rows.Scan(&d.TaskID, &d.ProjectID, &d.State, &d.URL, &d.Token, &pushed); err != nil {
			return nil, err
		}
		d.PushedState = nullToPtr(pushed)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkTaskPushed implements persistence.CompanionPushOutbox.
func (o *CompanionPushOutbox) MarkTaskPushed(ctx context.Context, taskID string, prev *string, state string) (bool, error) {
	return casPushed(ctx, o.db, `UPDATE a2a_push_configs SET pushed_state = ? WHERE task_id = ? AND pushed_state IS ?`, state, taskID, prev)
}

// DueActionPushes implements persistence.CompanionPushOutbox.
func (o *CompanionPushOutbox) DueActionPushes(ctx context.Context, limit int) ([]persistence.ActionPushDue, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.db.QueryContext(ctx, `
		SELECT a.task_id, a.project_id, a.action_id, a.action_kind, `+actionFrontStateSQL+`,
		       COALESCE(c.url, ''), COALESCE(c.token, ''), a.pushed_state
		FROM broker_actions a
		JOIN tasks t ON t.id = a.task_id
		LEFT JOIN a2a_push_configs c ON c.task_id = a.task_id
		WHERE t.creation_source = 'COMPANION'
		  AND a.status IN ('pending', 'approved', 'rejected', 'expired', 'executed', 'failed', 'unknown')
		  AND (a.pushed_state IS NULL OR a.pushed_state <> `+actionFrontStateSQL+`)
		ORDER BY a.created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ActionPushDue
	for rows.Next() {
		var d persistence.ActionPushDue
		var pushed sql.NullString
		if err := rows.Scan(&d.TaskID, &d.ProjectID, &d.ActionID, &d.Action, &d.State, &d.URL, &d.Token, &pushed); err != nil {
			return nil, err
		}
		d.PushedState = nullToPtr(pushed)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkActionPushed implements persistence.CompanionPushOutbox.
func (o *CompanionPushOutbox) MarkActionPushed(ctx context.Context, actionID string, prev *string, state string) (bool, error) {
	return casPushed(ctx, o.db, `UPDATE broker_actions SET pushed_state = ? WHERE action_id = ? AND pushed_state IS ?`, state, actionID, prev)
}

// casPushed runs the compare-and-set; SQLite's IS matches NULL to NULL.
func casPushed(ctx context.Context, db *sql.DB, query, state, id string, prev *string) (bool, error) {
	var p any
	if prev != nil {
		p = *prev
	}
	res, err := db.ExecContext(ctx, query, state, id, p)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func nullToPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

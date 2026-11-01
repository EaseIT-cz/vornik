package postgres

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskCredentialRepository implements persistence.TaskCredentialRepository
// against PostgreSQL. It backs credential carryover: an operator-facing
// access credential captured from a trusted tool's structured output
// (see https://docs.vornik.io),
// surfaced code-formatted + copyable in the completion notification and task
// detail.
type TaskCredentialRepository struct {
	db DBTX
}

// NewTaskCredentialRepository constructs the repo over db.
func NewTaskCredentialRepository(db DBTX) *TaskCredentialRepository {
	return &TaskCredentialRepository{db: db}
}

// Upsert records a captured credential, overwriting the value in place on a
// conflict of (task_id, execution_id, tool, artifact_url). ID and CreatedAt
// default when left zero.
func (r *TaskCredentialRepository) Upsert(ctx context.Context, cred *persistence.TaskCredential) error {
	if cred.ID == "" {
		cred.ID = persistence.GenerateID("taskcred")
	}
	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_credentials (
			id, task_id, execution_id, tool, label, value, artifact_url, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (task_id, execution_id, tool, artifact_url)
		DO UPDATE SET label = EXCLUDED.label, value = EXCLUDED.value, created_at = EXCLUDED.created_at`,
		cred.ID, cred.TaskID, cred.ExecutionID, cred.Tool, cred.Label, cred.Value, cred.ArtifactURL, cred.CreatedAt,
	)
	return mapDBError(err)
}

// ListAll implements persistence.TaskCredentialSealPass (#578 B3).
func (r *TaskCredentialRepository) ListAll(ctx context.Context) ([]*persistence.TaskCredential, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, tool, label, value, artifact_url, created_at
		FROM task_credentials`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TaskCredential
	for rows.Next() {
		var c persistence.TaskCredential
		if err := rows.Scan(&c.ID, &c.TaskID, &c.ExecutionID, &c.Tool, &c.Label, &c.Value, &c.ArtifactURL, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ReplaceValue implements persistence.TaskCredentialSealPass (#578 B3).
func (r *TaskCredentialRepository) ReplaceValue(ctx context.Context, id, prev, next string) (bool, error) {
	tag, err := r.db.ExecContext(ctx, `UPDATE task_credentials SET value = $1 WHERE id = $2 AND value = $3`, next, id, prev)
	if err != nil {
		return false, mapDBError(err)
	}
	n, err := tag.RowsAffected()
	return n == 1, err
}

// ListByTaskLatestExecution returns the credentials captured by the task's
// most-recently-capturing execution only, so a retry's stale credential is
// never surfaced. Empty when none.
func (r *TaskCredentialRepository) ListByTaskLatestExecution(ctx context.Context, taskID string) ([]*persistence.TaskCredential, error) {
	if taskID == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, tool, label, value, artifact_url, created_at
		FROM task_credentials
		WHERE task_id = $1
		  AND execution_id = (
		      SELECT execution_id FROM task_credentials
		      WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1
		  )
		ORDER BY created_at`, taskID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TaskCredential
	for rows.Next() {
		var c persistence.TaskCredential
		if err := rows.Scan(&c.ID, &c.TaskID, &c.ExecutionID, &c.Tool, &c.Label, &c.Value, &c.ArtifactURL, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

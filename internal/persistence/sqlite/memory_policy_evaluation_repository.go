package sqlite

// SQLite implementation of MemoryPolicyEvaluationRepository — the memory
// firewall's per-evaluation audit trail (postgres migration 80 + 102).
//
// Until 2026-10-13 this was a stub that discarded every row (GitHub #355), so
// the firewall's audit read empty on SQLite whatever had been evaluated. It is
// now a real store with the Postgres semantics, pinned by
// repotest.RunMemoryPolicyEvaluationSuite on both drivers. Note that nothing on
// SQLite produces rows today: the firewall evaluates during memory recall, and
// recall is Postgres-only (pgvector). The table records whatever is evaluated,
// so it stops being a silent discard the day a SQLite recall path exists.
//
// see LLD § https://docs.vornik.io §4.19

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
)

// MemoryPolicyEvaluationRepository persists firewall evaluation rows.
type MemoryPolicyEvaluationRepository struct {
	db *sql.DB
}

// NewMemoryPolicyEvaluationRepository wires the repository.
func NewMemoryPolicyEvaluationRepository(db *sql.DB) *MemoryPolicyEvaluationRepository {
	return &MemoryPolicyEvaluationRepository{db: db}
}

const policyEvalColumns = `id, project_id, COALESCE(tenant_id, ''), chunk_id,
       COALESCE(request_role, ''), COALESCE(request_purpose, ''),
       COALESCE(request_operator, ''), COALESCE(trace_id, ''),
       decision, COALESCE(policy_digest, ''), COALESCE(reason_detail, ''),
       evaluated_at`

// policyEvalLimit is the Postgres clamp: outside (0, 500] means 50.
func policyEvalLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 50
	}
	return limit
}

func nullIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// BatchInsert writes the rows in one multi-row INSERT. Empty input is a no-op.
func (r *MemoryPolicyEvaluationRepository) BatchInsert(ctx context.Context, rows []memoryfirewall.EvaluationRow) error {
	if r == nil || r.db == nil {
		return errors.New("memory_policy_evaluations: not configured")
	}
	if len(rows) == 0 {
		return nil
	}
	placeholders := make([]string, len(rows))
	args := make([]any, 0, len(rows)*12)
	for i, row := range rows {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args,
			row.ID, row.ProjectID, nullIfBlank(row.TenantID), row.ChunkID,
			nullIfBlank(row.RequestRole), nullIfBlank(row.RequestPurpose),
			nullIfBlank(row.RequestOperator), nullIfBlank(row.TraceID),
			string(row.Decision), nullIfBlank(row.PolicyDigest), nullIfBlank(row.ReasonDetail),
			sqliteTime(row.EvaluatedAt),
		)
	}
	q := `INSERT INTO memory_policy_evaluations
    (id, project_id, tenant_id, chunk_id, request_role, request_purpose,
     request_operator, trace_id, decision, policy_digest, reason_detail, evaluated_at)
VALUES ` + strings.Join(placeholders, ",")
	if _, err := r.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("memory_policy_evaluations: batch insert: %w", err)
	}
	return nil
}

// ListRecent returns a project's rows evaluated at or after since, newest
// first, optionally filtered to one decision.
func (r *MemoryPolicyEvaluationRepository) ListRecent(ctx context.Context, projectID, decisionFilter string, since time.Time, limit int) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	q := `SELECT ` + policyEvalColumns + `
FROM memory_policy_evaluations
WHERE project_id = ? AND evaluated_at >= ? AND (? = '' OR decision = ?)
ORDER BY evaluated_at DESC, id DESC
LIMIT ?`
	return r.list(ctx, "list", q, projectID, sqliteTime(since), decisionFilter, decisionFilter, policyEvalLimit(limit))
}

// ListByDigest returns every row recorded under a policy digest, newest first.
func (r *MemoryPolicyEvaluationRepository) ListByDigest(ctx context.Context, policyDigest string, limit int) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	q := `SELECT ` + policyEvalColumns + `
FROM memory_policy_evaluations
WHERE policy_digest = ?
ORDER BY evaluated_at DESC, id DESC
LIMIT ?`
	return r.list(ctx, "list by digest", q, policyDigest, policyEvalLimit(limit))
}

// ListByChunk returns one chunk's rows, newest first.
func (r *MemoryPolicyEvaluationRepository) ListByChunk(ctx context.Context, chunkID string, limit int) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	q := `SELECT ` + policyEvalColumns + `
FROM memory_policy_evaluations
WHERE chunk_id = ?
ORDER BY evaluated_at DESC, id DESC
LIMIT ?`
	return r.list(ctx, "list by chunk", q, chunkID, policyEvalLimit(limit))
}

func (r *MemoryPolicyEvaluationRepository) list(ctx context.Context, what, q string, args ...any) ([]memoryfirewall.EvaluationRow, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory_policy_evaluations: %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []memoryfirewall.EvaluationRow
	for rows.Next() {
		var row memoryfirewall.EvaluationRow
		var decision string
		var at sqlTime
		if err := rows.Scan(
			&row.ID, &row.ProjectID, &row.TenantID, &row.ChunkID,
			&row.RequestRole, &row.RequestPurpose, &row.RequestOperator, &row.TraceID,
			&decision, &row.PolicyDigest, &row.ReasonDetail, &at,
		); err != nil {
			return nil, fmt.Errorf("memory_policy_evaluations: scan: %w", err)
		}
		row.Decision = memoryfirewall.EvaluationDecision(decision)
		row.EvaluatedAt = at.Time
		out = append(out, row)
	}
	return out, rows.Err()
}

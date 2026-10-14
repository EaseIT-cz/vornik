package postgres

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AgentAdminRefusalRepository implements persistence.AgentAdminRefusalRepository
// over Postgres (agent-administered design §23.2);
// repotest.RunAgentAdminRefusalSuite keeps it equal to the SQLite one.
type AgentAdminRefusalRepository struct {
	db persistence.DBTX
}

// NewAgentAdminRefusalRepository wires the repository.
func NewAgentAdminRefusalRepository(db persistence.DBTX) *AgentAdminRefusalRepository {
	return &AgentAdminRefusalRepository{db: db}
}

var _ persistence.AgentAdminRefusalRepository = (*AgentAdminRefusalRepository)(nil)

// Record implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) Record(ctx context.Context, a persistence.AgentAdminRefusal) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO agent_admin_refusals (id, project_id, namespace, key_id, verb, rule, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, a.ID, a.ProjectID, a.Namespace, a.KeyID, a.Verb, a.Rule, a.CreatedAt.UTC())
	return mapDBError(err)
}

// ListRecent implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) ListRecent(ctx context.Context, namespace string, limit int) ([]persistence.AgentAdminRefusal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, project_id, namespace, key_id, verb, rule, created_at
		FROM agent_admin_refusals WHERE namespace = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, namespace, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentAdminRefusal
	for rows.Next() {
		var a persistence.AgentAdminRefusal
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Namespace, &a.KeyID, &a.Verb, &a.Rule, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountByRule implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) CountByRule(ctx context.Context, namespace string, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT rule, COUNT(*) FROM agent_admin_refusals
		WHERE namespace = $1 AND created_at >= $2 GROUP BY rule`, namespace, since.UTC())
	if err != nil {
		return nil, err
	}
	return scanRefusalCounts(rows)
}

// CountByNamespace implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) CountByNamespace(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT namespace, COUNT(*) FROM agent_admin_refusals
		WHERE created_at >= $1 GROUP BY namespace`, since.UTC())
	if err != nil {
		return nil, err
	}
	return scanRefusalCounts(rows)
}

func scanRefusalCounts(rows *sql.Rows) (map[string]int, error) {
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			k string
			n int
		)
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

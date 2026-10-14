package sqlite

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AgentAdminRefusalRepository implements persistence.AgentAdminRefusalRepository
// over SQLite (agent-administered design §23.2);
// repotest.RunAgentAdminRefusalSuite keeps it equal to the Postgres one.
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
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.ProjectID, a.Namespace, a.KeyID, a.Verb, a.Rule, sqliteTime(a.CreatedAt))
	return err
}

// ListRecent implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) ListRecent(ctx context.Context, namespace string, limit int) ([]persistence.AgentAdminRefusal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, project_id, namespace, key_id, verb, rule, created_at
		FROM agent_admin_refusals WHERE namespace = ? ORDER BY created_at DESC, id DESC LIMIT ?`, namespace, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentAdminRefusal
	for rows.Next() {
		var (
			a  persistence.AgentAdminRefusal
			at sqlTime
		)
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Namespace, &a.KeyID, &a.Verb, &a.Rule, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = at.Time
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountByRule implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) CountByRule(ctx context.Context, namespace string, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT rule, COUNT(*) FROM agent_admin_refusals
		WHERE namespace = ? AND created_at >= ? GROUP BY rule`, namespace, sqliteTime(since))
	if err != nil {
		return nil, err
	}
	return scanRefusalCounts(rows)
}

// CountByNamespace implements persistence.AgentAdminRefusalRepository.
func (r *AgentAdminRefusalRepository) CountByNamespace(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT namespace, COUNT(*) FROM agent_admin_refusals
		WHERE created_at >= ? GROUP BY namespace`, sqliteTime(since))
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

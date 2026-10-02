package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// AgentSecretRepository implements persistence.AgentSecretRepository over
// SQLite. It mirrors the Postgres implementation; repotest.RunAgentSecretSuite
// keeps the two honest.
type AgentSecretRepository struct {
	db *sql.DB
}

// NewAgentSecretRepository wires the repository.
func NewAgentSecretRepository(db *sql.DB) *AgentSecretRepository {
	return &AgentSecretRepository{db: db}
}

var _ persistence.AgentSecretRepository = (*AgentSecretRepository)(nil)

// Upsert implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) Upsert(ctx context.Context, row persistence.AgentSecretRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_secrets (namespace, name, kind, ciphertext, nonce, created_by_device, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (namespace, name) DO UPDATE SET
			kind = excluded.kind,
			ciphertext = excluded.ciphertext,
			nonce = excluded.nonce,
			created_by_device = excluded.created_by_device,
			updated_at = excluded.updated_at`,
		row.Namespace, row.Name, row.Kind, row.Ciphertext, row.Nonce, row.CreatedByDevice,
		sqliteTime(row.CreatedAt), sqliteTime(row.UpdatedAt))
	return err
}

// Get implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) Get(ctx context.Context, namespace, name string) (*persistence.AgentSecretRow, error) {
	var (
		out              persistence.AgentSecretRow
		created, updated sqlTime
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT namespace, name, kind, ciphertext, nonce, created_by_device, created_at, updated_at
		FROM agent_secrets WHERE namespace = ? AND name = ?`, namespace, name).
		Scan(&out.Namespace, &out.Name, &out.Kind, &out.Ciphertext, &out.Nonce, &out.CreatedByDevice, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.CreatedAt, out.UpdatedAt = created.Time, updated.Time
	return &out, nil
}

// List implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) List(ctx context.Context, namespace string) ([]persistence.AgentSecretRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT namespace, name, kind, created_by_device, created_at, updated_at
		FROM agent_secrets WHERE namespace = ? ORDER BY name`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentSecretRow
	for rows.Next() {
		var (
			row              persistence.AgentSecretRow
			created, updated sqlTime
		)
		if err := rows.Scan(&row.Namespace, &row.Name, &row.Kind, &row.CreatedByDevice, &created, &updated); err != nil {
			return nil, err
		}
		row.CreatedAt, row.UpdatedAt = created.Time, updated.Time
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListNamespaces implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) ListNamespaces(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT namespace FROM agent_secrets ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// Delete implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) Delete(ctx context.Context, namespace, name string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM agent_secrets WHERE namespace = ? AND name = ?`, namespace, name)
	return err
}

// CountAll implements persistence.AgentSecretRepository.
func (r *AgentSecretRepository) CountAll(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_secrets`).Scan(&n)
	return n, err
}

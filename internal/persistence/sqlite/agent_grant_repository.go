package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AgentGrantRepository implements persistence.AgentGrantRepository over
// SQLite; repotest.RunAgentGrantSuite keeps it equal to the Postgres one.
type AgentGrantRepository struct {
	db *sql.DB
}

// NewAgentGrantRepository wires the repository.
func NewAgentGrantRepository(db *sql.DB) *AgentGrantRepository {
	return &AgentGrantRepository{db: db}
}

var _ persistence.AgentGrantRepository = (*AgentGrantRepository)(nil)

func grantToolsJSON(xs []string) string {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

func grantToolsFrom(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// UpsertIntegration implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) UpsertIntegration(ctx context.Context, a persistence.AgentIntegrationApproval) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_integration_approvals
			(namespace, project_id, integration, kind, url, read_tools, write_tools, read_pending, approved_by_device, approved_at, removed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (project_id, integration) DO UPDATE SET
			namespace = excluded.namespace, kind = excluded.kind, url = excluded.url,
			read_tools = excluded.read_tools, write_tools = excluded.write_tools,
			read_pending = excluded.read_pending, approved_by_device = excluded.approved_by_device,
			approved_at = excluded.approved_at, removed_at = NULL`,
		a.Namespace, a.ProjectID, a.Integration, a.Kind, a.URL, grantToolsJSON(a.ReadTools), grantToolsJSON(a.WriteTools),
		boolToInt(a.ReadPending), a.ApprovedByDevice, sqliteTime(a.ApprovedAt))
	return err
}

const agentIntegrationCols = `namespace, project_id, integration, kind, url, read_tools, write_tools, read_pending, approved_by_device, approved_at, removed_at`

func scanAgentIntegration(s interface{ Scan(...interface{}) error }) (persistence.AgentIntegrationApproval, error) {
	var (
		a           persistence.AgentIntegrationApproval
		read, write string
		pending     int64
		approved    sqlTime
		removed     sqlNullTime
	)
	if err := s.Scan(&a.Namespace, &a.ProjectID, &a.Integration, &a.Kind, &a.URL, &read, &write, &pending, &a.ApprovedByDevice, &approved, &removed); err != nil {
		return a, err
	}
	a.ReadTools, a.WriteTools, a.ReadPending, a.ApprovedAt = grantToolsFrom(read), grantToolsFrom(write), pending != 0, approved.Time
	if removed.Valid {
		t := removed.Time
		a.RemovedAt = &t
	}
	return a, nil
}

// GetIntegration implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) GetIntegration(ctx context.Context, projectID, integration string) (*persistence.AgentIntegrationApproval, error) {
	a, err := scanAgentIntegration(r.db.QueryRowContext(ctx, `SELECT `+agentIntegrationCols+` FROM agent_integration_approvals WHERE project_id = ? AND integration = ?`, projectID, integration))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListIntegrations implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) ListIntegrations(ctx context.Context, namespace string) ([]persistence.AgentIntegrationApproval, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+agentIntegrationCols+` FROM agent_integration_approvals WHERE namespace = ? ORDER BY project_id, integration`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentIntegrationApproval
	for rows.Next() {
		a, err := scanAgentIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkIntegrationRemoved implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) MarkIntegrationRemoved(ctx context.Context, projectID, integration string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_integration_approvals SET removed_at = ? WHERE project_id = ? AND integration = ? AND removed_at IS NULL`,
		sqliteTime(at), projectID, integration)
	return err
}

// UpsertWorkflowReach implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) UpsertWorkflowReach(ctx context.Context, a persistence.AgentWorkflowApproval) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_workflow_approvals (workflow_id, namespace, project_id, reach_hash, approved_by_device, approved_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (workflow_id) DO UPDATE SET
			namespace = excluded.namespace, project_id = excluded.project_id, reach_hash = excluded.reach_hash,
			approved_by_device = excluded.approved_by_device, approved_at = excluded.approved_at`,
		a.WorkflowID, a.Namespace, a.ProjectID, a.ReachHash, a.ApprovedByDevice, sqliteTime(a.ApprovedAt))
	return err
}

func scanAgentWorkflow(s interface{ Scan(...interface{}) error }) (persistence.AgentWorkflowApproval, error) {
	var (
		a  persistence.AgentWorkflowApproval
		at sqlTime
	)
	err := s.Scan(&a.WorkflowID, &a.Namespace, &a.ProjectID, &a.ReachHash, &a.ApprovedByDevice, &at)
	a.ApprovedAt = at.Time
	return a, err
}

// GetWorkflowReach implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) GetWorkflowReach(ctx context.Context, workflowID string) (*persistence.AgentWorkflowApproval, error) {
	a, err := scanAgentWorkflow(r.db.QueryRowContext(ctx, `SELECT workflow_id, namespace, project_id, reach_hash, approved_by_device, approved_at FROM agent_workflow_approvals WHERE workflow_id = ?`, workflowID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListWorkflowReach implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) ListWorkflowReach(ctx context.Context, namespace string) ([]persistence.AgentWorkflowApproval, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT workflow_id, namespace, project_id, reach_hash, approved_by_device, approved_at FROM agent_workflow_approvals WHERE namespace = ? ORDER BY workflow_id`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentWorkflowApproval
	for rows.Next() {
		a, err := scanAgentWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteWorkflowReach implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) DeleteWorkflowReach(ctx context.Context, workflowID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM agent_workflow_approvals WHERE workflow_id = ?`, workflowID)
	return err
}

// UpsertCeiling implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) UpsertCeiling(ctx context.Context, b persistence.AgentNamespaceBudget) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_namespace_budgets (namespace, ceiling_usd, approved_by_device, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (namespace) DO UPDATE SET ceiling_usd = excluded.ceiling_usd,
			approved_by_device = excluded.approved_by_device, updated_at = excluded.updated_at`,
		b.Namespace, b.CeilingUSD, b.ApprovedByDevice, sqliteTime(b.UpdatedAt))
	return err
}

// GetCeiling implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) GetCeiling(ctx context.Context, namespace string) (*persistence.AgentNamespaceBudget, error) {
	var (
		b  persistence.AgentNamespaceBudget
		at sqlTime
	)
	err := r.db.QueryRowContext(ctx, `SELECT namespace, ceiling_usd, approved_by_device, updated_at FROM agent_namespace_budgets WHERE namespace = ?`, namespace).
		Scan(&b.Namespace, &b.CeilingUSD, &b.ApprovedByDevice, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.UpdatedAt = at.Time
	return &b, nil
}

// Model destination approvals (agent-administered design §18.6 item 2 in
// detail): repotest.RunAgentModelDestinationSuite keeps both drivers equal.

const agentModelDestinationCols = `namespace, destination, approved_by_device, approved_at, removed_at`

// UpsertModelDestination implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) UpsertModelDestination(ctx context.Context, a persistence.AgentModelDestinationApproval) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_model_provider_approvals (namespace, destination, approved_by_device, approved_at, removed_at)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT (namespace, destination) DO UPDATE SET
			approved_by_device = excluded.approved_by_device, approved_at = excluded.approved_at, removed_at = NULL`,
		a.Namespace, a.Destination, a.ApprovedByDevice, sqliteTime(a.ApprovedAt))
	return err
}

func scanModelDestination(s interface{ Scan(...interface{}) error }) (persistence.AgentModelDestinationApproval, error) {
	var (
		a        persistence.AgentModelDestinationApproval
		approved sqlTime
		removed  sqlNullTime
	)
	if err := s.Scan(&a.Namespace, &a.Destination, &a.ApprovedByDevice, &approved, &removed); err != nil {
		return a, err
	}
	a.ApprovedAt = approved.Time
	if removed.Valid {
		t := removed.Time
		a.RemovedAt = &t
	}
	return a, nil
}

// GetModelDestination implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) GetModelDestination(ctx context.Context, namespace, destination string) (*persistence.AgentModelDestinationApproval, error) {
	a, err := scanModelDestination(r.db.QueryRowContext(ctx, `SELECT `+agentModelDestinationCols+` FROM agent_model_provider_approvals WHERE namespace = ? AND destination = ?`, namespace, destination))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListModelDestinations implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) ListModelDestinations(ctx context.Context, namespace string) ([]persistence.AgentModelDestinationApproval, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+agentModelDestinationCols+` FROM agent_model_provider_approvals WHERE namespace = ? ORDER BY destination`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentModelDestinationApproval
	for rows.Next() {
		a, err := scanModelDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkModelDestinationRemoved implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) MarkModelDestinationRemoved(ctx context.Context, namespace, destination string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE agent_model_provider_approvals SET removed_at = ? WHERE namespace = ? AND destination = ? AND removed_at IS NULL`,
		sqliteTime(at), namespace, destination)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

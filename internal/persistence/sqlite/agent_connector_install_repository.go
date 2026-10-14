package sqlite

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// Connector installs (agent-administered design §19.14.8 item 6; GitHub
// #314): repotest.RunAgentConnectorInstallSuite keeps both drivers equal.

// UpsertConnectorInstall implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) UpsertConnectorInstall(ctx context.Context, c persistence.AgentConnectorInstall) error {
	label := c.AccountLabel
	if label == "" {
		label = persistence.AccountLabelDefault
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_connector_installs (workflow_id, namespace, project_id, connector, account_label, device_id, installed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workflow_id) DO UPDATE SET
			namespace = excluded.namespace, project_id = excluded.project_id, connector = excluded.connector,
			account_label = excluded.account_label, device_id = excluded.device_id, installed_at = excluded.installed_at`,
		c.WorkflowID, c.Namespace, c.ProjectID, c.Connector, label, c.DeviceID, sqliteTime(c.InstalledAt))
	return err
}

// ListConnectorInstalls implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) ListConnectorInstalls(ctx context.Context, namespace string) ([]persistence.AgentConnectorInstall, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT workflow_id, namespace, project_id, connector, account_label, device_id, installed_at
		FROM agent_connector_installs WHERE namespace = ? ORDER BY workflow_id`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentConnectorInstall
	for rows.Next() {
		var (
			c  persistence.AgentConnectorInstall
			at sqlTime
		)
		if err := rows.Scan(&c.WorkflowID, &c.Namespace, &c.ProjectID, &c.Connector, &c.AccountLabel, &c.DeviceID, &at); err != nil {
			return nil, err
		}
		c.InstalledAt = at.Time
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnectorInstall implements persistence.AgentGrantRepository.
func (r *AgentGrantRepository) DeleteConnectorInstall(ctx context.Context, workflowID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM agent_connector_installs WHERE workflow_id = ?`, workflowID)
	return err
}

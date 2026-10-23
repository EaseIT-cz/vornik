package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// The store of held agent read tools (agent-administered design §6.5,
// amendment 2026-10-21, GitHub #481): the MCP manager's
// mcp.ReadSuspensionStore over the agent grant repository. The suspended
// set lives on the approval row; only a re-add the person approves on the
// device clears it (UpsertIntegration's re-add branch).

// readSuspensionStore adapts persistence.AgentGrantRepository.
type readSuspensionStore struct {
	grants persistence.AgentGrantRepository
}

// LoadReadSuspensions implements mcp.ReadSuspensionStore.
func (s readSuspensionStore) LoadReadSuspensions(ctx context.Context) (map[string]map[string][]string, error) {
	rows, err := s.grants.ListSuspendedIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]string{}
	for _, r := range rows {
		if r.Kind != "mcp" || len(r.SuspendedTools) == 0 {
			continue
		}
		if out[r.ProjectID] == nil {
			out[r.ProjectID] = map[string][]string{}
		}
		out[r.ProjectID][r.Integration] = r.SuspendedTools
	}
	return out, nil
}

// SuspendReadTools implements mcp.ReadSuspensionStore.
func (s readSuspensionStore) SuspendReadTools(ctx context.Context, projectID, server string, tools []string, listedAt time.Time) error {
	err := s.grants.SuspendIntegrationTools(ctx, projectID, server, tools, listedAt)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return fmt.Errorf("%w: %w", mcp.ErrSuspensionNoApproval, err)
	case errors.Is(err, persistence.ErrStaleListing):
		return fmt.Errorf("%w: %w", mcp.ErrSuspensionMoot, err)
	}
	return err
}

// wireReadSuspensions sets the manager's store and reads it. It runs where
// the manager is reconciled (initMCPWithBudget), before SyncProjects, so
// every reload reads the stored set before it installs clients; a failed
// read leaves agent read calls refused until the re-list loop reads it.
func (c *Container) wireReadSuspensions(ctx context.Context) {
	if c == nil || c.mcpManager == nil || c.repos == nil {
		return // no persistence at all (a bare unit-test container)
	}
	if c.repos.AgentGrants == nil {
		// A daemon with repositories but no grant repository would run
		// agent reads with no stickiness: refuse them instead (code
		// review 3450 F2).
		c.mcpManager.SetReadSuspensions(mcp.UnavailableReadSuspensions{Why: "no agent grant repository"})
		_ = c.mcpManager.LoadReadSuspensions(ctx)
		return
	}
	c.mcpManager.SetReadSuspensions(readSuspensionStore{grants: c.repos.AgentGrants})
	_ = c.mcpManager.LoadReadSuspensions(ctx) // logged by the manager
}

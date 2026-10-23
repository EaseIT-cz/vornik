package service

import (
	"context"

	"vornik.io/vornik/internal/config"
)

// startMCPReconnector runs the MCP manager's failed-connect reconnector
// (MCP failed-connect recovery design, D2) and wires its notify-only outage
// alert to the operator-alert channel. Every node runs one: each node dials
// its own clients, so this is deliberately not leader-gated.
func (c *Container) startMCPReconnector(ctx context.Context) {
	if c == nil || c.mcpManager == nil {
		return
	}
	if n := c.operatorAlertNotifier(); n != nil {
		c.mcpManager.SetOutageNotifier(func(ctx context.Context, text string) error {
			n.NotifyOperator(ctx, "MCP server connection", text)
			return nil
		})
	}
	go c.mcpManager.RunReconnector(ctx)
	// The periodic re-list of agent read servers (agent-administered design
	// §6.5, amendment #481; operator decision A7): every node, like the
	// reconnector, since each dials its own clients.
	interval := config.DefaultAgentReadRelistInterval
	if c.Config != nil {
		interval = c.Config.MCP.AgentReadRelist()
	}
	go c.mcpManager.RunAgentReadRelist(ctx, interval)
}

package api

import (
	"context"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// The bounds of the agent tool list's wait for still-starting MCP servers
// (MCP failed-connect recovery design, phase 2 P3): wait up to 10 s, and only
// for a server whose outage began less than 30 s ago. A server down for
// longer is already in the doctor, the gauge and the alert, and delays nothing.
const (
	mcpStartingMaxWait = 10 * time.Second
	mcpStartingWindow  = 30 * time.Second
)

// startingServerWaiter is implemented by *mcp.Manager.
type startingServerWaiter interface {
	WaitForStartingServers(ctx context.Context, projectID string, maxWait, startingWindow time.Duration) []mcp.PendingServer
}

// WaitForStartingServers delegates to the external executor when it can wait;
// otherwise there is nothing to wait for.
func (c *ComposedMCPExecutor) WaitForStartingServers(ctx context.Context, projectID string, maxWait, startingWindow time.Duration) []mcp.PendingServer {
	if w, ok := c.External.(startingServerWaiter); ok {
		return w.WaitForStartingServers(ctx, projectID, maxWait, startingWindow)
	}
	return nil
}

// waitForStartingMCPServers runs the bounded wait before the agent's tool list
// is built, and logs every server still starting when it gives up: the step
// then runs without that server's tools, and the log line is what says so.
// Telling the model needs a field in the agent bridge's contract (named gap).
func (s *Server) waitForStartingMCPServers(ctx context.Context, projectID string) {
	w, ok := s.mcpExecutor.(startingServerWaiter)
	if !ok {
		return
	}
	for _, p := range w.WaitForStartingServers(ctx, projectID, mcpStartingMaxWait, mcpStartingWindow) {
		s.logger.Warn().
			Str("project", p.ProjectID).
			Str("server", p.Server).
			Time("pending_since", p.Since).
			Str("last_error", p.LastError).
			Msg("mcp: tool list answered while a server was still starting; this step runs without its tools")
	}
}

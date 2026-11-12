package service

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// registerTradingHandlers adds the Enterprise trading system-step handlers
// (trading.place_approved, GitHub #739): the step that places a trading
// tick's approved orders in place of the executor model. It is reachable
// only from a workflow step an operator configured; no chat, API or MCP
// request can invoke it. Community has no factory and registers nothing.
//
// The caller and the audit writer are resolved at call time. The container
// rebuilds the MCP manager on config reload and the redacting audit
// repository on an observability rebuild; a handler holding a stale audit
// handle could write unredacted rows.
func (c *Container) registerTradingHandlers(reg *executor.SystemHandlerRegistry) {
	if c.providers.TradingSystemHandlers == nil {
		return
	}
	for _, h := range c.providers.TradingSystemHandlers(lateMCPCaller{c}, lateToolAudit{c}) {
		reg.Register(h)
	}
}

// lateMCPCaller is mcp.Manager.CallToolOnce on the container's current
// manager.
type lateMCPCaller struct{ c *Container }

func (l lateMCPCaller) CallToolOnce(ctx context.Context, projectID, name, args string) (string, bool, error) {
	if l.c.mcpManager == nil {
		return "", false, fmt.Errorf("%w: the MCP manager is not running", mcp.ErrNotSent)
	}
	return l.c.mcpManager.CallToolOnce(ctx, projectID, name, args)
}

// lateToolAudit writes through the container's current tool-audit
// repository. initToolAuditRedaction wraps that repository in the redacting
// decorator (wireToolAuditSeam, container_audit_redaction.go) before any
// task runs, and re-wraps it after an observability rebuild.
type lateToolAudit struct{ c *Container }

func (l lateToolAudit) Log(ctx context.Context, e *persistence.ToolAuditEntry) error {
	if l.c.repos == nil || l.c.repos.ToolAudit == nil {
		l.c.Logger.Warn().Str("tool", e.ToolName).Str("task_id", e.TaskID).
			Msg("trading: tool audit repository unavailable; the audit row for this broker call is lost")
		return nil
	}
	return l.c.repos.ToolAudit.Log(ctx, e)
}

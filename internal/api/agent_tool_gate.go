package api

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// N4 for agent-namespace projects (agent-administered Vornik design §10.1,
// §7.3; plan P3.8). It is BLOCKING and FAIL-CLOSED, unlike the operator-swarm
// gate (roleAllowsMCPTool), which admits on every resolution gap. A tool
// call from an agent project must:
//   - carry a task, whose role resolves to a non-empty allowedTools;
//   - name a tool that allowlist admits (the strict matcher);
//   - be a broker-safe built-in, or an MCP tool of an integration approved
//     for THIS project (agent_integration_approvals: not removed, not
//     read-pending) and in that integration's approved read or write set.
//
// The project file's own lists are not trusted on their own: a hand edit
// gains nothing, because the approval table is written only by a device.

// WithAgentGrants wires the approval tables the agent tool gate checks.
func WithAgentGrants(repo persistence.AgentGrantRepository) ServerOption {
	return func(srv *Server) { srv.agentGrants = repo }
}

// agentToolGateCounts is the gate's denominator: how many agent-project
// calls it examined, and how many it refused.
type agentToolGateCounts struct {
	Examined, Refused atomic.Int64
}

// agentToolRefusal returns why an agent project's tool call is refused, or ""
// to admit it. ok=false means the project is not an agent project and the
// operator gate applies instead.
func (s *Server) agentToolRefusal(ctx context.Context, projectID, taskID, tool string) (reason string, agent bool) {
	if _, isAgent := agentns.FromID(projectID); !isAgent {
		return "", false
	}
	s.agentToolCounts.Examined.Add(1)
	reason = s.agentToolCheck(ctx, projectID, taskID, tool)
	if reason != "" {
		s.agentToolCounts.Refused.Add(1)
		s.logger.Warn().Str("project", projectID).Str("task_id", taskID).Str("tool", tool).Str("reason", reason).
			Int64("examined", s.agentToolCounts.Examined.Load()).Int64("refused", s.agentToolCounts.Refused.Load()).
			Msg("agent tool gate: refused")
	}
	return reason, true
}

func (s *Server) agentToolCheck(ctx context.Context, projectID, taskID, tool string) string {
	if taskID == "" {
		return "a tool call from an agent project must carry its task"
	}
	resolve := s.roleToolAllowlistReason
	if s.agentRoleAllowlistForTest != nil {
		resolve = s.agentRoleAllowlistForTest
	}
	allowed, gap := resolve(ctx, taskID)
	if len(allowed) == 0 {
		return fmt.Sprintf("the calling role's allowlist did not resolve (%s)", gap)
	}
	if !agenttools.RoleAllowsTool(allowed, tool) {
		return "the tool is not in the calling role's allowedTools"
	}
	if !strings.HasPrefix(tool, "mcp__") {
		if registry.IsBrokerSafeBuiltin(tool) {
			return ""
		}
		if tool == agentadmin.QueryAPITool {
			return s.queryAPIRefusal(ctx, projectID)
		}
		return "only workspace and clock built-ins are available to agent projects"
	}
	return agentadmin.ToolApprovalRefusal(ctx, s.agentGrants, projectID, tool, false)
}

// queryAPIRefusal admits query_api for an agent project with at least one
// API whose approval is live (plan P4.5); the client then judges the
// provider and the method.
func (s *Server) queryAPIRefusal(ctx context.Context, projectID string) string {
	if s.agentGrants == nil || s.projectRegistry == nil {
		return "the approval tables are not wired"
	}
	p := s.projectRegistry.GetProject(projectID)
	if p == nil {
		return "the project is not loaded"
	}
	for _, a := range p.APIs {
		row, err := s.agentGrants.GetIntegration(ctx, projectID, a.Name)
		if err == nil && row.RemovedAt == nil && row.Kind == "api" {
			return ""
		}
	}
	return "query_api needs an approved API in this project"
}

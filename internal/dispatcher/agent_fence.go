package dispatcher

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/chat"
)

// agentProjectFence hides agent-namespace projects from the chat dispatcher
// (agent-administered Vornik design §10.1; review 20261002-a048 F1). An
// agent project's integrations are approved for its own workflow roles and
// reached only through CallMCPTool's fail-closed gate. The chat has no task
// and no role to check, so for an agent project it lists nothing and calls
// nothing.
type agentProjectFence struct{ inner MCPExecutor }

// fenceAgentProjects wraps m; nil stays nil, and a fence is not wrapped twice.
func fenceAgentProjects(m MCPExecutor) MCPExecutor {
	if m == nil {
		return nil
	}
	if _, ok := m.(agentProjectFence); ok {
		return m
	}
	return agentProjectFence{inner: m}
}

func (f agentProjectFence) Tools(projectID string) []chat.Tool {
	if _, agent := agentns.FromID(projectID); agent {
		return nil
	}
	return f.inner.Tools(projectID)
}

func (f agentProjectFence) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if _, agent := agentns.FromID(projectID); agent {
		return "", fmt.Errorf("%s belongs to agent project %q; its integrations run only inside that project's own workflows", qualifiedName, projectID)
	}
	return f.inner.Execute(ctx, projectID, qualifiedName, argsJSON)
}

// scraperFence is the same fence for the narrower scraper executor.
type scraperFence struct{ inner scraperMCPExecutor }

func (f scraperFence) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if _, agent := agentns.FromID(projectID); agent {
		return "", fmt.Errorf("%s belongs to agent project %q; its integrations run only inside that project's own workflows", qualifiedName, projectID)
	}
	return f.inner.Execute(ctx, projectID, qualifiedName, argsJSON)
}

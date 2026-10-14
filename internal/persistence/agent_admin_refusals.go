package persistence

import (
	"context"
	"time"
)

// AgentAdminRefusal is one refused call made with an agent-admin key on the
// companion endpoint (agent-administered design §23.2, GitHub #172). It holds
// who, what and which rule, never the reason text or any input value: the
// reason quotes the agent's input, which may carry data.
type AgentAdminRefusal struct {
	ID string
	// ProjectID is the key's home project ("<ns>--home"): the retention sweep
	// prunes this table per project, on the tool_audit_log window.
	ProjectID string
	Namespace string
	// KeyID is the API key's row ID, not its secret or prefix.
	KeyID string
	// Verb is the companion tool called, or "unknown_tool".
	Verb string
	// Rule is one of agentadmin.RefusalRules.
	Rule      string
	CreatedAt time.Time
}

// AgentAdminRefusalRepository persists agent_admin_refusals (migration 229).
// repotest.RunAgentAdminRefusalSuite keeps the two drivers equal.
type AgentAdminRefusalRepository interface {
	Record(ctx context.Context, r AgentAdminRefusal) error
	// ListRecent returns a namespace's newest rows first, at most limit.
	ListRecent(ctx context.Context, namespace string, limit int) ([]AgentAdminRefusal, error)
	// CountByRule counts a namespace's rows at or after since, by rule.
	CountByRule(ctx context.Context, namespace string, since time.Time) (map[string]int, error)
	// CountByNamespace counts every namespace's rows at or after since.
	CountByNamespace(ctx context.Context, since time.Time) (map[string]int, error)
}

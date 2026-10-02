package api

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

type memGrants struct {
	persistence.AgentGrantRepository
	rows map[string]persistence.AgentIntegrationApproval
}

func (m memGrants) GetIntegration(_ context.Context, project, name string) (*persistence.AgentIntegrationApproval, error) {
	a, ok := m.rows[project+"/"+name]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &a, nil
}

// Design §10.1, §7.3 (plan P3.8): for agent projects the gate is blocking
// and fail-closed against the approval table; the file's own lists are not
// trusted. Control: agentToolCheck. Without it: a hand-edited role holding
// an unapproved write tool is admitted (the operator gate admits on gaps).
func TestAgentToolGate(t *testing.T) {
	removed := time.Now()
	grants := memGrants{rows: map[string]persistence.AgentIntegrationApproval{
		"hermes--fin/fio":  {ReadTools: []string{"balance", "history"}, WriteTools: []string{}},
		"hermes--fin/mail": {ReadTools: []string{"read"}, RemovedAt: &removed},
		"hermes--fin/new":  {ReadPending: true, ReadTools: []string{"x"}}, // tools listed, not yet approved
	}}
	role := []string{"file_read", "web_fetch", "mcp__fio__balance", "mcp__fio__transfer", "mcp__mail__read", "mcp__new__x", "mcp__other__y"}
	srv := &Server{logger: zerolog.Nop(), agentGrants: grants,
		agentRoleAllowlistForTest: func(_ context.Context, taskID string) ([]string, mcpGapReason) {
			if taskID == "gap" {
				return nil, mcpGapNoTaskID
			}
			return role, ""
		}}
	ctx := context.Background()
	cases := []struct {
		project, task, tool string
		admit               bool
	}{
		{"hermes--fin", "t", "mcp__fio__balance", true},
		{"hermes--fin", "t", "file_read", true},
		{"hermes--fin", "t", "mcp__fio__transfer", false}, // in the role, not approved (a hand edit)
		{"hermes--fin", "t", "web_fetch", false},          // a networked built-in
		{"hermes--fin", "t", "mcp__mail__read", false},    // approval removed
		{"hermes--fin", "t", "mcp__new__x", false},        // tools pending
		{"hermes--fin", "t", "mcp__other__y", false},      // never approved
		{"hermes--fin", "t", "mcp__fio__not_in_role", false},
		{"hermes--fin", "t", "mcp__fio__history", false},   // approved for the server, not in this role
		{"hermes--fin", "gap", "mcp__fio__balance", false}, // resolution gap: fail closed
		{"hermes--fin", "", "mcp__fio__balance", false},    // no task
	}
	for _, c := range cases {
		reason, agent := srv.agentToolRefusal(ctx, c.project, c.task, c.tool)
		if !agent {
			t.Fatalf("%s not treated as an agent project", c.project)
		}
		if (reason == "") != c.admit {
			t.Errorf("%s %s task %q: reason %q, want admit=%v", c.project, c.tool, c.task, reason, c.admit)
		}
	}
	if _, agent := srv.agentToolRefusal(ctx, "assistant", "gap", "mcp__x__y"); agent {
		t.Error("an operator project went through the agent gate; it keeps its own fail-open gate")
	}
	ex, ref := srv.agentToolCounts.Examined.Load(), srv.agentToolCounts.Refused.Load()
	if ex != int64(len(cases)) || ref != int64(len(cases)-2) {
		t.Errorf("denominator: examined %d refused %d, want %d and %d", ex, ref, len(cases), len(cases)-2)
	}
	t.Logf("examined %d agent-project calls, refused %d", ex, ref)
}

// The test double keeps the repository's miss contract.
func TestMemGrants_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "AgentGrantRepository.GetIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return memGrants{}.GetIntegration(context.Background(), "p", "absent")
	})
}

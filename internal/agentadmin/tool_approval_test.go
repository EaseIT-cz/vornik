package agentadmin

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

type oneGrant struct {
	persistence.AgentGrantRepository
	row *persistence.AgentIntegrationApproval
}

func (g oneGrant) GetIntegration(_ context.Context, projectID, integration string) (*persistence.AgentIntegrationApproval, error) {
	if g.row == nil || g.row.ProjectID != projectID || g.row.Integration != integration {
		return nil, persistence.ErrNotFound
	}
	return g.row, nil
}

// Design §7.3: the approval table alone admits an agent project's MCP tool,
// and a write needs the WRITE set. Control: ToolApprovalRefusal.
func TestToolApprovalRefusal(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	row := func() *persistence.AgentIntegrationApproval {
		return &persistence.AgentIntegrationApproval{ProjectID: "hermes--fin", Integration: "bank",
			ReadTools: []string{"balance"}, WriteTools: []string{"pay"}}
	}
	cases := []struct {
		name  string
		grant persistence.AgentGrantRepository
		tool  string
		write bool
		admit bool
	}{
		{"read tool, read call", oneGrant{row: row()}, "mcp__bank__balance", false, true},
		// P4.3b: no direct writes in a broker project. A role reads only;
		// the worker writes only through the -write entry.
		{"write tool, read call", oneGrant{row: row()}, "mcp__bank__pay", false, false},
		{"write entry, read call", oneGrant{row: row()}, "mcp__bank-write__pay", false, false},
		{"write entry, write call", oneGrant{row: row()}, "mcp__bank-write__pay", true, true},
		{"read entry, write call", oneGrant{row: row()}, "mcp__bank__pay", true, false},
		{"read tool, write call", oneGrant{row: row()}, "mcp__bank__balance", true, false},
		{"unknown tool", oneGrant{row: row()}, "mcp__bank__drain", false, false},
		{"other server", oneGrant{row: row()}, "mcp__mail__balance", false, false},
		{"no tables", nil, "mcp__bank__balance", false, false},
		{"not an mcp tool", oneGrant{row: row()}, "balance", false, false},
		{"empty server", oneGrant{row: row()}, "mcp____balance", false, false},
		{"removed", oneGrant{row: func() *persistence.AgentIntegrationApproval { r := row(); r.RemovedAt = &now; return r }()}, "mcp__bank__balance", false, false},
		{"read pending, write call", oneGrant{row: func() *persistence.AgentIntegrationApproval { r := row(); r.ReadPending = true; return r }()}, "mcp__bank__pay", true, false},
	}
	// P4.8: an API write is the worker's only, against the live WRITE set.
	apiRow := func() *persistence.AgentIntegrationApproval {
		return &persistence.AgentIntegrationApproval{ProjectID: "hermes--fin", Integration: "pay", Kind: "api",
			ReadTools: []string{"GET"}, WriteTools: []string{"POST"}}
	}
	cases = append(cases, []struct {
		name  string
		grant persistence.AgentGrantRepository
		tool  string
		write bool
		admit bool
	}{
		{"api write by the worker", oneGrant{row: apiRow()}, "api:pay:POST:/payments", true, true},
		{"api write by a role", oneGrant{row: apiRow()}, "api:pay:POST:/payments", false, false},
		{"api method not in the write set", oneGrant{row: apiRow()}, "api:pay:DELETE:/x", true, false},
		{"api removed", oneGrant{row: func() *persistence.AgentIntegrationApproval { r := apiRow(); r.RemovedAt = &now; return r }()}, "api:pay:POST:/p", true, false},
	}...)
	for _, c := range cases {
		got := ToolApprovalRefusal(ctx, c.grant, "hermes--fin", c.tool, c.write)
		if (got == "") != c.admit {
			t.Errorf("%s: refusal %q, want admit=%v", c.name, got, c.admit)
		}
	}
}

// The test double keeps the repository's miss contract.
func TestOneGrant_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "AgentGrantRepository.GetIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return oneGrant{}.GetIntegration(context.Background(), "p", "absent")
	})
}

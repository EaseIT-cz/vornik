package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #119 item 8 (2026-10-08): the approval stores exactly the account
// the device approved (agent-administered design §22.2.2, "approval
// mismatch"): the label and the agent-described identity come from the
// approved payload's grant, and the loaded project file carries the same
// label the reach signature will bind.
func TestApproval_AccountStoredIsTheApprovedOne(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail-work", URL: "https://mail.example/mcp",
		Auth:    agentadmin.MCPAuthInput{Mode: "static", Credential: "MAIL_WORK"},
		Account: &agentadmin.AccountInput{Label: "work", Identity: "alice@corp.example"}})
	f.approve(res)
	got, err := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--comms", "mail-work")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "work" || got.AccountIdentity != "alice@corp.example" || got.IdentitySource != "agent" {
		t.Fatalf("stored account %q/%q/%q, want the approved work/alice@corp.example/agent", got.AccountLabel, got.AccountIdentity, got.IdentitySource)
	}
	p := f.c.Registry.GetProject("hermes--comms")
	if p == nil || len(p.MCP.Servers) != 1 || p.MCP.Servers[0].Account != "work" {
		t.Fatalf("the loaded project does not carry the label: %+v", p)
	}

	// A default connection stores the default account with no identity.
	res = f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "news", URL: "https://news.example/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "NEWS"}})
	f.approve(res)
	got, err = f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--comms", "news")
	if err != nil || got.AccountLabel != "default" || got.AccountIdentity != "" || got.IdentitySource != "" {
		t.Fatalf("a default connection stored %+v (%v)", got, err)
	}
}

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #119 item 8 (2026-10-08): list_my_setup shows each connection's
// host and account and groups a project's connections to one host
// (agent-administered design §22.2.9), so an agent can tell two accounts
// at one provider apart and ask the user which. A provider-reported
// identity is never shown to the agent (§22.2.7).

func accountsSetupFixture(t *testing.T) *agentAdminFixture {
	t.Helper()
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail-work", URL: "https://mail.example/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "MAIL_WORK"}, Account: &agentadmin.AccountInput{Label: "work", Identity: "alice@corp.example"}}))
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail-personal", URL: "https://mail.example/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "MAIL_PERSONAL"}}))
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "comms", Name: "news", BaseURL: "https://news.example",
		Methods: []string{"GET"}}))
	return f
}

func TestListMySetup_GroupsAccountsByHost(t *testing.T) {
	f := accountsSetupFixture(t)
	p := projectView(setupOf(t, f), "hermes--comms")
	if p == nil {
		t.Fatal("no project")
	}
	if len(p.ConnectionsByHost) != 1 || p.ConnectionsByHost[0].Host != "mail.example" || len(p.ConnectionsByHost[0].Connections) != 2 {
		t.Fatalf("connections by host: %+v", p.ConnectionsByHost)
	}
	labels := map[string]string{}
	for _, c := range p.ConnectionsByHost[0].Connections {
		labels[c.Name] = c.Account.Label
	}
	if labels["mail-work"] != "work" || labels["mail-personal"] != "default" {
		t.Fatalf("accounts at mail.example: %+v", labels)
	}
	for _, s := range p.Servers {
		if s.Host != "mail.example" {
			t.Fatalf("server %s host %q", s.Name, s.Host)
		}
	}
	if p.APIs[0].Host != "news.example" || p.APIs[0].Account.Label != "default" {
		t.Fatalf("API: %+v", p.APIs[0])
	}
	for _, c := range p.Credentials {
		if c.Name == "MAIL_WORK" && c.Account != "work" {
			t.Fatalf("credential account: %+v", c)
		}
	}
}

func TestListMySetup_ShowsIdentityAndSource(t *testing.T) {
	f := accountsSetupFixture(t)
	p := projectView(setupOf(t, f), "hermes--comms")
	for _, s := range p.Servers {
		if s.Name == "mail-work" && (s.Account.Identity != "alice@corp.example" || s.Account.IdentitySource != "agent") {
			t.Fatalf("an agent-described identity: %+v", s.Account)
		}
	}
}

func TestListMySetup_ProviderIdentityNotShownToAgent(t *testing.T) {
	f := accountsSetupFixture(t)
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(context.Background(), "hermes--comms", "mail-work", "provider-alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	v := setupOf(t, f)
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "provider-alice@corp.example") {
		t.Fatalf("list_my_setup carries a provider identity: %s", raw)
	}
	for _, s := range projectView(v, "hermes--comms").Servers {
		if s.Name == "mail-work" && (s.Account.IdentitySource != "provider" || s.Account.Identity != "") {
			t.Fatalf("a provider-identified account: %+v", s.Account)
		}
	}
}

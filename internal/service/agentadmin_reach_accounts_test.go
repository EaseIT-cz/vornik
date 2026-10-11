package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #119 item 8 (2026-10-08): the executor's reach check binds the
// account of each connection a workflow reaches (agent-administered design
// §22.2.4). A workflow approved over the default account is refused
// REACH_NOT_APPROVED once the connection it reads acts as another account.
func TestReach_ExecutorRefusesOnAccountChange(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "MAIL_TOKEN"}}))
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "MAIL_TOKEN", "secret", []byte("v"), f.device.ID); err != nil {
		t.Fatal(err)
	}
	cfgs := filepath.Join(f.cfgDir, "configs")
	edit := func(path, old, repl string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(string(b), old, repl, 1)
		if edited == string(b) {
			t.Fatalf("the edit of %s did not apply; the template changed shape", path)
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := f.c.ConfigReloader.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}
	// The worker reads the server (a hand edit stands in for the approved
	// tools, which need the credential and a listing).
	edit(filepath.Join(cfgs, "swarms", "hermes--comms.md"), `          - "file_write"`, "          - \"file_write\"\n          - \"mcp__mail__read\"")
	f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "comms", Slug: "digest",
		Steps:  []agentadmin.StepInput{{Name: "read", Role: "worker", Instructions: "Read."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`)}))
	if err := f.c.verifyAgentReach(ctx, "hermes--comms", "hermes--comms--digest"); err != nil {
		t.Fatalf("the approved workflow was refused: %v", err)
	}
	// The connection now acts as the work account.
	edit(filepath.Join(cfgs, "projects", "hermes--comms.yaml"), `      url: "https://mail.example/mcp"`, "      url: \"https://mail.example/mcp\"\n      account: \"work\"")
	if f.c.Registry.GetProject("hermes--comms").MCP.Servers[0].Account != "work" {
		t.Fatal("the edited project did not load")
	}
	err = f.c.verifyAgentReach(ctx, "hermes--comms", "hermes--comms--digest")
	if err == nil || !strings.Contains(err.Error(), "changed what it returns or can reach") {
		t.Fatalf("a workflow whose connection changed account was allowed to run: %v", err)
	}
}

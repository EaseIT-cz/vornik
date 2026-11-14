package service

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
)

// GitHub #625 item 5 (agent-administered design, "Follow-up 2026-11-12 (#625
// item 5)"): a server the daemon refuses to register vanished from the hub's
// MCP section with its reason only in the journal. Every refusal of both
// passes is now recorded with its scope and reason.
//
// Found at: review (#623's "Not in this change"); a user observes it at
// production. Regression test at: unit (the two passes the hub and the doctor
// read). Lower because: the hub's rendering is tested in internal/ui against
// the same record; no daemon is needed to tell the passes apart.

func refusalFor(rs []mcp.Refusal, scope, server string) (mcp.Refusal, bool) {
	for _, r := range rs {
		if r.Scope == scope && r.Server == server {
			return r, true
		}
	}
	return mcp.Refusal{}, false
}

func TestMCPRefusals625_DaemonAuthUnresolvable(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{
			Name: "n8n", Transport: "streamable-http", URL: "https://n8n.example.com/mcp",
			Auth: mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://MISSING_625_TOKEN"},
		}}}},
	}
	if got := c.daemonMCPServerConfigs(); len(got) != 0 {
		t.Fatalf("an unresolvable daemon server must not register, got %+v", got)
	}
	r, ok := refusalFor(c.mcpRefusals(), "", "n8n")
	if !ok {
		t.Fatalf("daemon refusal not recorded: %+v", c.mcpRefusals())
	}
	if !strings.Contains(r.Reason, "MISSING_625_TOKEN") {
		t.Errorf("reason %q does not name the secret", r.Reason)
	}

	// Fixed configuration: the next pass clears the row.
	t.Setenv("MISSING_625_TOKEN", "value-625")
	if got := c.daemonMCPServerConfigs(); len(got) != 1 {
		t.Fatalf("the fixed server did not register: %+v", got)
	}
	if rs := c.mcpRefusals(); len(rs) != 0 {
		t.Errorf("refusals after the fix = %+v, want none", rs)
	}
}

func TestMCPRefusals625_ProjectPassRecordsAllThreeKinds(t *testing.T) {
	t.Setenv("PROJECT_TOKEN_625", "ptok")
	t.Setenv("DAEMON_TOKEN_625", "dtok")
	c := containerWithProjectMCP(t, `permissions:
  secrets: ["PROJECT_TOKEN_625", "ABSENT_625"]
mcp:
  servers:
    - name: "both"
      auth:
        mode: static
        value_from: "secret://PROJECT_TOKEN_625"
    - name: "dangling"
    - name: "noauth"
      transport: "streamable-http"
      url: "https://x.example.com/mcp"
      auth:
        mode: static
        value_from: "secret://ABSENT_625"
`,
		config.MCPServerConfig{
			Name: "both", Transport: "streamable-http", URL: "https://both.example.com/mcp",
			Auth: mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://DAEMON_TOKEN_625"},
		})
	c.mcpDesiredServers()
	rs := c.mcpRefusals()
	for _, server := range []string{"both", "dangling", "noauth"} {
		if _, ok := refusalFor(rs, "test-project", server); !ok {
			t.Errorf("project refusal %s not recorded: %+v", server, rs)
		}
	}
	if r, _ := refusalFor(rs, "test-project", "noauth"); !strings.Contains(r.Reason, "ABSENT_625") {
		t.Errorf("auth refusal reason %q does not name the secret", r.Reason)
	}
	// No reason carries a secret's value: both values are set in this test.
	for _, r := range rs {
		for _, v := range []string{"ptok", "dtok"} {
			if strings.Contains(r.Reason, v) {
				t.Errorf("refusal %s/%s carries a secret value: %q", r.Scope, r.Server, r.Reason)
			}
		}
	}
	// The doctor's list now carries the unresolvable-auth refusal it missed.
	var doctorHasAuth bool
	for _, w := range c.mcpWithheldServers() {
		if strings.HasPrefix(w, "test-project/noauth:") {
			doctorHasAuth = true
		}
	}
	if !doctorHasAuth || len(c.mcpWithheldServers()) != 3 {
		t.Errorf("doctor withheld = %v, want three entries including test-project/noauth", c.mcpWithheldServers())
	}
}

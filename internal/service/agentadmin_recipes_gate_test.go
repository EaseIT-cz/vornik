package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/registry"
)

// Review 20261003-6b46 F6: a credential reference outside the project's
// namespace (which the loader refuses, §7.5, but the gate must not trust)
// is reported by its bare name: no other namespace, and no reference form,
// reaches the delegate message or a log line.
func TestMissingCredential_NamesOnlyTheBareName(t *testing.T) {
	c := &Container{}
	p := &registry.Project{ID: "hermes--personal"}
	p.MCP.Servers = []registry.MCPServerConfig{{Name: "mail"}}
	p.MCP.Servers[0].Auth.ValueFrom = "secret://other/MAIL_TOKEN"
	got := c.missingCredential(context.Background(), p, agentadmin.ReachSignature{Integrations: []string{"mail"}})
	if got != "MAIL_TOKEN" {
		t.Fatalf("missing credential reported as %q", got)
	}
}

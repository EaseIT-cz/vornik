package agentadmin

import (
	"strings"
	"testing"
)

// treeWithPendingServer has project "fin" with a read-pending server "bank".
func treeWithPendingServer(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "fin", Purpose: "finance"}))
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "bank", URL: "https://bank.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "FIO"}})
	if !c.Grant.Integrations[0].ReadPending {
		t.Fatal("the fixture server is not read-pending")
	}
	tr.apply(c)
	return tr
}

// Plan P4.3 (P3 amendment 5): once a server is connected, its listed tools
// are approved by a daemon-internal verb. The approval grants exactly the
// listed tools, read-pending off, and the rendered server allows exactly
// them. Control: approveServerTools.
func TestApproveServerTools(t *testing.T) {
	tr := treeWithPendingServer(t)
	in := ApproveServerToolsInput{Project: "hermes--fin", Server: "bank", Tools: []string{"statement", "balance", "balance"}}
	c := tr.render(VerbApproveServerTools, in)
	if c.Class != Widening {
		t.Fatalf("class %v: %s", c.Class, c.Reason)
	}
	g := c.Grant.Integrations
	if len(g) != 1 || g[0].ReadPending || strings.Join(g[0].Read, ",") != "balance,statement" || len(g[0].Write) != 0 {
		t.Fatalf("grant %+v", g)
	}
	for _, want := range []string{"bank", "bank.example", "2 tools", "balance, statement", "hermes--fin"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	tr.apply(c)
	srv := tr.state().Projects["hermes--fin"].Servers[0]
	if strings.Join(srv.Tools, ",") != "balance,statement" || srv.AuthRef != "secret://hermes/FIO" {
		t.Fatalf("rendered server %+v", srv)
	}

	// Not pending any more: a second approval is refused.
	if c := tr.render(VerbApproveServerTools, in); c.Class != Refused {
		t.Fatal("a server that is not waiting was re-approved")
	}
	tr2 := treeWithPendingServer(t)
	for name, bad := range map[string]ApproveServerToolsInput{
		"unknown server":  {Project: "hermes--fin", Server: "mail", Tools: []string{"x"}},
		"unknown project": {Project: "hermes--zzz", Server: "bank", Tools: []string{"x"}},
		"no tools":        {Project: "hermes--fin", Server: "bank"},
		"bad tool name":   {Project: "hermes--fin", Server: "bank", Tools: []string{"a b"}},
		"other namespace": {Project: "codex--fin", Server: "bank", Tools: []string{"x"}},
	} {
		if c := tr2.render(VerbApproveServerTools, bad); c.Class != Refused {
			t.Errorf("%s: accepted", name)
		}
	}
}

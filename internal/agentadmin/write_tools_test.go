package agentadmin

import (
	"strings"
	"testing"
)

// Plan P4.3b (broker invariants): write tools render as a broker_write
// sibling "<name>-write" that no role may hold; the read entry carries the
// read tools only; the grant splits read and write; removing the
// integration removes both entries. Control: the write branch of
// addMCPServer, approveServerTools and removeIntegration.
func TestAddMCPServer_WriteToolsAreAProposableSibling(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.advert["https://mail.example/mcp"] = []string{"read_inbox", "send"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp", WriteTools: []string{"send"}})
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if g.Name != "mail" || strings.Join(g.Read, ",") != "read_inbox" || strings.Join(g.Write, ",") != "send" {
		t.Fatalf("grant %+v", g)
	}
	if !strings.Contains(c.Sentence, "propose changes with: send") || !strings.Contains(c.Sentence, "approval before it is made") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	tr.apply(c)
	servers := tr.state().Projects["hermes--comms"].Servers
	if len(servers) != 2 {
		t.Fatalf("servers %+v", servers)
	}
	read, write := servers[0], servers[1]
	if read.Name != "mail" || read.Write || strings.Join(read.Tools, ",") != "read_inbox" {
		t.Fatalf("read entry %+v", read)
	}
	if write.Name != "mail-write" || !write.Write || strings.Join(write.Tools, ",") != "send" || write.URL != read.URL || write.AuthRef != read.AuthRef {
		t.Fatalf("write entry %+v", write)
	}

	// A role can never hold the write entry's tool.
	if c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "comms", Roles: []RoleInput{{Name: "worker", Instructions: "x",
		Tools: []string{"mcp__mail-write__send"}}}}); c.Class != Refused {
		t.Fatal("a role was given a write tool")
	}
	if c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "comms", Roles: []RoleInput{{Name: "worker", Instructions: "x",
		Tools: []string{"mcp__mail__send"}}}}); c.Class != Refused {
		t.Fatal("a role was given a write tool through the read entry")
	}

	// An unadvertised write tool is refused.
	tr.advert["https://other.example/mcp"] = []string{"read"}
	if c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "other", URL: "https://other.example/mcp", WriteTools: []string{"wipe"}}); c.Class != Refused {
		t.Fatal("an unadvertised write tool was accepted")
	}

	// Removing the integration removes both entries; the -write entry is not
	// removable on its own.
	if c := tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "comms", ID: "mail-write"}); c.Class != Refused {
		t.Fatal("the write entry was removed on its own")
	}
	rm := tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "comms", ID: "mail"})
	tr.apply(rm)
	if got := tr.state().Projects["hermes--comms"].Servers; len(got) != 0 {
		t.Fatalf("after removal: %+v", got)
	}
}

// A read-pending server with write tools: the tools approval splits the
// listing into read and write, and refuses when a requested write tool is
// not offered.
func TestApproveServerTools_SplitsWrites(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("comms")
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL"}, WriteTools: []string{"send"}})
	if !c.Grant.Integrations[0].ReadPending || strings.Join(c.Grant.Integrations[0].Write, ",") != "send" {
		t.Fatalf("grant %+v", c.Grant)
	}
	tr.apply(c)
	if c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--comms", Server: "mail", Tools: []string{"read_inbox"}}); c.Class != Refused {
		t.Fatal("approved although the write tool is not offered")
	}
	c = tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--comms", Server: "mail", Tools: []string{"read_inbox", "send"}})
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if strings.Join(g.Read, ",") != "read_inbox" || strings.Join(g.Write, ",") != "send" || g.ReadPending {
		t.Fatalf("grant %+v", g)
	}
	tr.apply(c)
	for _, s := range tr.state().Projects["hermes--comms"].Servers {
		if s.Name == "mail" && strings.Join(s.Tools, ",") != "read_inbox" {
			t.Fatalf("the read entry allows %v", s.Tools)
		}
	}
}

// Review 20261002-5d00 F1: the tools approval refuses when the write entry's
// tools no longer equal the approved write set (a hand edit), rather than
// recording a write set the registry does not declare.
func TestApproveServerTools_RefusesWriteEntryDrift(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL"}, WriteTools: []string{"send"}}))
	for path, content := range tr.files {
		if strings.HasPrefix(path, "projects/") {
			tr.files[path] = strings.Replace(content, `        - "send"`, `        - "send"
        - "delete"`, 1)
		}
	}
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--comms", Server: "mail", Tools: []string{"read_inbox", "send", "delete"}})
	if c.Class != Refused || !strings.Contains(c.Reason, "write entry") {
		t.Fatalf("a drifted write entry was approved: %v %s", c.Class, c.Reason)
	}
}

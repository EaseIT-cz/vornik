package agentadmin

import (
	"strings"
	"testing"
)

// Plan P4.4: add_mcp_server with auth oauth renders an OAuth server that is
// read-pending until a person connects it on the phone; request_credential
// (kind oauth) names its token slot OAUTH_<SERVER>. Control: the oauth
// branches of mcpCredential, the project template and requestCredential.
func TestAddMCPServer_OAuth(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("work")
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "work", Name: "jira-cloud", URL: "https://mcp.atlassian.example/v1",
		Auth: MCPAuthInput{Mode: "oauth", Scopes: []string{"read:jira-work", "offline_access"}}})
	tr.mustClass(c, Widening)
	if !c.Grant.Integrations[0].ReadPending {
		t.Fatal("an OAuth server must be read-pending until connected")
	}
	if !strings.Contains(c.Ops[0].Content, "mode: oauth") || !strings.Contains(c.Ops[0].Content, `"read:jira-work"`) {
		t.Fatalf("rendered:\n%s", c.Ops[0].Content)
	}
	if !strings.Contains(c.Sentence, "signing in") || !strings.Contains(c.Sentence, "read:jira-work") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	tr.apply(c)
	srv := tr.state().Projects["hermes--work"].Servers[0]
	if !srv.OAuth || strings.Join(srv.Scopes, ",") != "offline_access,read:jira-work" {
		t.Fatalf("state %+v", srv)
	}

	slot := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "work", Name: OAuthCredentialName("jira-cloud"), Purpose: "read issues", Kind: "oauth"})
	tr.mustClass(slot, Widening)
	if slot.Slot.Kind != "oauth" || slot.Slot.Server != "jira-cloud" || OAuthCredentialName("jira-cloud") != "OAUTH_JIRA_CLOUD" {
		t.Fatalf("slot %+v", slot.Slot)
	}
	if !strings.Contains(slot.Sentence, "Connect") {
		t.Fatalf("sentence: %s", slot.Sentence)
	}
	for name, in := range map[string]RequestCredentialInput{
		"secret kind for an oauth server": {Project: "work", Name: OAuthCredentialName("jira-cloud"), Purpose: "x", Kind: "secret"},
		"oauth kind, unknown token":       {Project: "work", Name: "OAUTH_NOPE", Purpose: "x", Kind: "oauth"},
	} {
		if c := tr.render(VerbRequestCredential, in); c.Class != Refused {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, bad := range [][]string{{"a b"}, {strings.Repeat("s", 200)}, make([]string, 21)} {
		in := AddMCPServerInput{Project: "work", Name: "x", URL: "https://x.example", Auth: MCPAuthInput{Mode: "oauth", Scopes: bad}}
		if c := tr.render(VerbAddMCPServer, in); c.Class != Refused {
			t.Errorf("scopes %q accepted", bad)
		}
	}
}

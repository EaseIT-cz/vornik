package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// plainHosts is a list of plain entries, as a string list loads.
func plainHosts(hosts ...string) []dialguard.PrivateHost {
	out := make([]dialguard.PrivateHost, len(hosts))
	for i, h := range hosts {
		out[i] = dialguard.PrivateHost{Host: h}
	}
	return out
}

// GitHub #403 (agent-administered design part 06, §6.10). Found at: review
// (the issue); a user observes it at the approval sentence on the approver
// device, which the renderer produces, so the tests sit here. An agent
// server or API on a host the operator lists in agent_admin.private_api_hosts
// may reach a private address once approved (§6.7, §6.8), but the approval
// sentence did not say so: the approver had to remember the list.

const intranetKey = "agent_admin.private_api_hosts"

func TestAddMCPServer_403_SentenceNamesAListedHost(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.privateHosts = plainHosts("mcp.lan.example")
	tr.project("fin")
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "gws", URL: "https://MCP.lan.example./mcp"})
	tr.mustClass(c, Widening)
	for _, want := range []string{"MCP.lan.example.", intranetKey, "private network", "never this machine"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
}

func TestAddMCPServer_403_NoClauseWithoutTheGrant(t *testing.T) {
	for name, tc := range map[string]struct {
		hosts []dialguard.PrivateHost
		url   string
	}{
		"empty list":        {nil, "https://mcp.lan.example/mcp"},
		"unlisted host":     {plainHosts("mcp.lan.example"), "https://other.lan.example/mcp"},
		"listed host, http": {plainHosts("mcp.lan.example"), "http://mcp.lan.example/mcp"},
		// An IP-literal entry grants since #403 (§6.12); a loopback one
		// cannot be listed, so it never grants.
		"loopback entry": {plainHosts("127.0.0.1"), "https://127.0.0.1/mcp"},
	} {
		tr := newTree(t, "hermes")
		tr.privateHosts = tc.hosts
		tr.project("fin")
		c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "gws", URL: tc.url})
		// add_mcp_server refuses plain http to any host but this machine,
		// so that case is refused before a sentence; it must not claim a
		// grant either way.
		if c.Class == Refused && name != "listed host, http" {
			t.Fatalf("%s: refused: %s", name, c.Reason)
		}
		if strings.Contains(c.Sentence, intranetKey) || strings.Contains(c.Reason, intranetKey) {
			t.Errorf("%s: sentence claims an intranet grant: %s", name, c.Sentence)
		}
	}
}

func TestAddAPI_403_SentenceNamesAListedHost(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.privateHosts = plainHosts("gitlab.corp.example")
	tr.project("finance")
	in := AddAPIInput{Project: "finance", Name: "gitlab", BaseURL: "https://gitlab.corp.example/api/v4", Methods: []string{"GET"}}
	c := tr.render(VerbAddAPI, in)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "gitlab.corp.example is on your list of intranet hosts ("+intranetKey+")") {
		t.Errorf("sentence: %s", c.Sentence)
	}
	tr2 := newTree(t, "hermes")
	tr2.project("finance")
	if c := tr2.render(VerbAddAPI, in); strings.Contains(c.Sentence, intranetKey) {
		t.Errorf("no list, but the sentence claims a grant: %s", c.Sentence)
	}
}

func TestInstallRecipe_403_SentenceNamesAListedServerHost(t *testing.T) {
	tr := shippedTree(t)
	tr.privateHosts = plainHosts("mail.lan.example")
	in := shippedVars("inbox-digest")
	in.Variables["mail_server_url"] = json.RawMessage(`"https://mail.lan.example/mcp"`)
	c := tr.render(VerbInstallRecipe, in)
	if c.Class == Refused {
		t.Fatalf("refused: %s", c.Reason)
	}
	if !strings.Contains(c.Sentence, "mail.lan.example is on your list of intranet hosts") {
		t.Errorf("sentence: %s", c.Sentence)
	}
	if n := strings.Count(c.Sentence, intranetKey); n != 1 {
		t.Errorf("the key appears %d times, want 1: %s", n, c.Sentence)
	}
}

func TestIntranetSentence_403_OneClausePerHost(t *testing.T) {
	hosts := plainHosts("a.lan.example", "b.lan.example")
	got := intranetSentence(hosts, "https://a.lan.example/x", "https://A.lan.example/y", "https://b.lan.example/", "https://c.example/", "http://b.lan.example/")
	if strings.Count(got, "a.lan.example") != 1 || !strings.Contains(got, "b.lan.example") || strings.Contains(got, "c.example") {
		t.Fatalf("got %q", got)
	}
	// Plain http on a listed host is admitted to public addresses only, so
	// it gets no clause whatever verb names it (review aac2 F1).
	if intranetSentence(hosts, "https://c.example/") != "" || intranetSentence(nil, "https://a.lan.example/") != "" ||
		intranetSentence(hosts, "http://a.lan.example/") != "" {
		t.Fatal("a clause without a grant")
	}
}

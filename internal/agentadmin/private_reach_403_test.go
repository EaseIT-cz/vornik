package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #403 (agent-administered design part 06, §6.12): the operator's
// answers. The approval records which agent_admin.private_api_hosts entry
// granted a new destination (IntegrationGrant.PrivateReach), the sentence
// names the same text, and PrivateReachMoved refuses an apply whose live
// grant differs. Found at: review (§6.10's operator questions); the approver
// observes the sentence and the record, which the renderer produces.

func entry403(t *testing.T, host string, pins []string, projects ...string) dialguard.PrivateHost {
	t.Helper()
	e, err := dialguard.ParsePrivateHost(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pins {
		pin, err := dialguard.ParsePin(p)
		if err != nil {
			t.Fatal(err)
		}
		e.Addresses = append(e.Addresses, pin)
	}
	e.Projects = projects
	return e
}

func TestAddMCPServer_403_RecordsTheGrantingEntry(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.privateHosts = []dialguard.PrivateHost{entry403(t, "nas.lan:8443", []string{"10.0.0.0/24"})}
	tr.project("fin")
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "nas", URL: "https://nas.lan:8443/mcp"})
	tr.mustClass(c, Widening)
	if got := c.Grant.Integrations[0].PrivateReach; got != "nas.lan:8443 within 10.0.0.0/24" {
		t.Errorf("private_reach = %q", got)
	}
	if !strings.Contains(c.Sentence, "nas.lan:8443 is on your list of intranet hosts (agent_admin.private_api_hosts), only at 10.0.0.0/24:") {
		t.Errorf("sentence: %s", c.Sentence)
	}
	// Another port of the host is not granted, so nothing is recorded.
	c = tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "nas2", URL: "https://nas.lan/mcp"})
	if got := c.Grant.Integrations[0].PrivateReach; got != "" || strings.Contains(c.Sentence, intranetKey) {
		t.Errorf("an ungranted port recorded %q: %s", got, c.Sentence)
	}
}

func TestAddAPI_403_RecordsTheGrantAndIsScopedByProject(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.privateHosts = []dialguard.PrivateHost{entry403(t, "10.0.0.5", nil, "hermes--finance")}
	tr.project("finance")
	tr.project("other")
	in := AddAPIInput{Project: "finance", Name: "nas", BaseURL: "https://10.0.0.5/api", Methods: []string{"GET"}}
	c := tr.render(VerbAddAPI, in)
	tr.mustClass(c, Widening)
	if got := c.Grant.Integrations[0].PrivateReach; got != "10.0.0.5" {
		t.Errorf("private_reach = %q", got)
	}
	in.Project = "other"
	c = tr.render(VerbAddAPI, in)
	if got := c.Grant.Integrations[0].PrivateReach; got != "" || strings.Contains(c.Sentence, intranetKey) {
		t.Errorf("a project the entry does not serve got %q: %s", got, c.Sentence)
	}
}

func TestInstallRecipe_403_RecordsTheGrantOfANewServer(t *testing.T) {
	tr := shippedTree(t)
	tr.privateHosts = []dialguard.PrivateHost{entry403(t, "mail.lan.example", nil)}
	in := shippedVars("inbox-digest")
	in.Variables["mail_server_url"] = json.RawMessage(`"https://mail.lan.example/mcp"`)
	c := tr.render(VerbInstallRecipe, in)
	if c.Class == Refused {
		t.Fatalf("refused: %s", c.Reason)
	}
	found := false
	for _, g := range c.Grant.Integrations {
		if strings.Contains(g.URL, "mail.lan.example") {
			found = true
			if g.PrivateReach != "mail.lan.example" {
				t.Errorf("%s: private_reach = %q", g.Name, g.PrivateReach)
			}
		}
	}
	if !found {
		t.Fatalf("no grant for the mail server: %+v", c.Grant.Integrations)
	}
}

// The apply check (§6.12 item 7): a new destination whose live grant differs
// from the recorded one is refused, both ways; an existing integration of the
// same name and URL is not a new destination and is skipped.
func TestPrivateReachMoved_403(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	in := AddAPIInput{Project: "finance", Name: "gitlab", BaseURL: "https://gitlab.lan/api", Methods: []string{"GET"}}
	filedUnlisted := tr.render(VerbAddAPI, in)
	tr.privateHosts = []dialguard.PrivateHost{entry403(t, "gitlab.lan", nil)}
	if why := PrivateReachMoved(tr.state(), filedUnlisted.Grant.Integrations); !strings.Contains(why, "list of intranet hosts changed") ||
		!strings.Contains(why, "gitlab.lan") {
		t.Errorf("filed unlisted, now listed: %q", why)
	}
	filedListed := tr.render(VerbAddAPI, in)
	if why := PrivateReachMoved(tr.state(), filedListed.Grant.Integrations); why != "" {
		t.Errorf("same list: %q", why)
	}
	// Pins changed between filing and approval: the text differs.
	pinned := entry403(t, "gitlab.lan", []string{"10.0.0.0/24"})
	tr.privateHosts = []dialguard.PrivateHost{pinned}
	if why := PrivateReachMoved(tr.state(), filedListed.Grant.Integrations); why == "" {
		t.Error("pins added after filing: not refused")
	}
	tr.privateHosts = nil
	if why := PrivateReachMoved(tr.state(), filedListed.Grant.Integrations); why == "" {
		t.Error("filed listed, now unlisted: not refused")
	}
	// Applied: the integration is live, so a later grant naming it (a tools
	// approval, a recipe's tools change) is not checked.
	tr.apply(filedUnlisted)
	tr.privateHosts = []dialguard.PrivateHost{entry403(t, "gitlab.lan", nil)}
	if why := PrivateReachMoved(tr.state(), filedUnlisted.Grant.Integrations); why != "" {
		t.Errorf("an existing integration was re-checked: %q", why)
	}
	// Same name, another URL: not the live integration, so it is checked.
	moved := filedUnlisted.Grant.Integrations[0]
	moved.URL = "https://gitlab.lan:8443/api"
	if why := PrivateReachMoved(tr.state(), []IntegrationGrant{moved}); why == "" {
		t.Error("a grant whose URL differs from the live integration was skipped")
	}
}

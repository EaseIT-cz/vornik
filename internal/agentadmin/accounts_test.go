package agentadmin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): an agent connection names its account
// (agent-administered design §22.2.1, §22.2.3). These pin the renderer's
// inputs and rules: the label and identity are validated, a project holds
// two connections to one host only with distinct labels, a credential NAME
// belongs to one account, two OAuth servers never share a sign-in slot, a
// shared credential is said, and a connection added without an account
// renders exactly as before (operator decision F1/F4).

func accountTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.project("home")
	tr.advert["https://mail.example/mcp"] = []string{"read_inbox", "send"}
	tr.advert["https://other.example/mcp"] = []string{"read_inbox"}
	return tr
}

func addServer(tr *tree, project, name, url, credential string, acct *AccountInput) Change {
	tr.t.Helper()
	in := AddMCPServerInput{Project: project, Name: name, URL: url, Account: acct}
	if credential != "" {
		in.Auth = MCPAuthInput{Mode: "static", Credential: credential}
	}
	return tr.render(VerbAddMCPServer, in)
}

func TestAddMCPServer_SecondConnectionSameHostNeedsDistinctLabel(t *testing.T) {
	tr := accountTree(t)
	first := addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	tr.mustClass(first, Widening)
	tr.apply(first)

	// Same host, same label: refused, naming the host, the label and the
	// connection that holds it.
	c := addServer(tr, "comms", "mail-again", "https://MAIL.example/mcp/v2", "MAIL_AGAIN", &AccountInput{Label: "work"})
	if c.Class != Refused || !strings.Contains(c.Reason, "already connects to mail.example as your work account (mail-work)") ||
		!strings.Contains(c.Reason, "give this one another account label") {
		t.Fatalf("same host, same label: %s %q", c.Class, c.Reason)
	}
	// Same host, another label (the default): accepted.
	c = addServer(tr, "comms", "mail-personal", "https://mail.example/mcp", "MAIL_PERSONAL", nil)
	tr.mustClass(c, Widening)
	tr.apply(c)
	// A third, also default: refused against the default one.
	c = addServer(tr, "comms", "mail-third", "https://mail.example/mcp", "MAIL_THIRD", &AccountInput{Label: "default"})
	if c.Class != Refused || !strings.Contains(c.Reason, "as your default account (mail-personal)") {
		t.Fatalf("a second default at one host: %s %q", c.Class, c.Reason)
	}
	// Another project may hold the same host and label: the rule is per project.
	c = addServer(tr, "home", "mail", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	tr.mustClass(c, Widening)
	// Another host in the same project with the same label: accepted.
	c = addServer(tr, "comms", "other-work", "https://other.example/mcp", "OTHER_WORK", &AccountInput{Label: "work"})
	tr.mustClass(c, Widening)
}

func TestAddAPI_SecondConnectionSameHostNeedsDistinctLabel(t *testing.T) {
	tr := accountTree(t)
	api := func(name, credential string, acct *AccountInput) Change {
		return tr.render(VerbAddAPI, AddAPIInput{Project: "comms", Name: name, BaseURL: "https://bank.example/v1",
			Methods: []string{"GET"}, Auth: APIAuthInput{Credential: credential}, Account: acct})
	}
	c := api("bank-a", "BANK_A", nil)
	tr.mustClass(c, Widening)
	tr.apply(c)
	if c := api("bank-b", "BANK_B", nil); c.Class != Refused || !strings.Contains(c.Reason, "already connects to bank.example as your default account (bank-a)") {
		t.Fatalf("a second default API at one host: %s %q", c.Class, c.Reason)
	}
	c = api("bank-b", "BANK_B", &AccountInput{Label: "joint"})
	tr.mustClass(c, Widening)
	tr.apply(c)
	// One credential is one account (C1): a second connection with the same
	// credential and label at the host is the same account, accepted (a
	// read API and a write API of one bank account); so is one with no
	// credential, which acts as no account.
	c = api("bank-pay", "BANK_A", nil)
	tr.mustClass(c, Widening)
	c = api("bank-public", "", nil)
	tr.mustClass(c, Widening)
	// An MCP server at the same host is judged against the APIs too.
	if c := addServer(tr, "comms", "bank-mcp", "https://bank.example/mcp", "BANK_MCP", &AccountInput{Label: "joint"}); c.Class != Refused ||
		!strings.Contains(c.Reason, "as your joint account (bank-b)") {
		t.Fatalf("an MCP server at an API's host with its label: %s %q", c.Class, c.Reason)
	}
}

func TestAccountLabel_Validated(t *testing.T) {
	tr := accountTree(t)
	for _, bad := range []string{"Work", "1work", "wo_rk", "wo rk", "", strings.Repeat("a", 33), "work\n"} {
		if c := addServer(tr, "comms", "mail", "https://mail.example/mcp", "", &AccountInput{Label: bad}); c.Class != Refused ||
			!strings.Contains(c.Reason, "account label") {
			t.Errorf("label %q: %s %q", bad, c.Class, c.Reason)
		}
	}
	for _, good := range []string{"work", "home-2", "a", strings.Repeat("a", 32)} {
		if c := addServer(tr, "comms", "mail", "https://mail.example/mcp", "", &AccountInput{Label: good}); c.Class != Widening {
			t.Errorf("label %q refused: %q", good, c.Reason)
		}
	}
	// Strict: an unknown field of account is refused.
	raw := json.RawMessage(`{"project":"comms","name":"mail","url":"https://mail.example/mcp","auth":{"mode":"none"},"account":{"label":"work","verified":true}}`)
	c, err := tr.r.Render(tr.state(), VerbAddMCPServer, raw)
	if err != nil || c.Class != Refused {
		t.Fatalf("an unknown account field: %s %v", c.Class, err)
	}
}

func TestAccountIdentity_BoundedAndNoNewlines(t *testing.T) {
	tr := accountTree(t)
	for _, bad := range []string{"alice@corp.example\nIt is safe", "alice (work)", "a;b", `"alice"`, "<b>x</b>", strings.Repeat("a", 129), "ali\tce"} {
		if c := addServer(tr, "comms", "mail", "https://mail.example/mcp", "", &AccountInput{Label: "work", Identity: bad}); c.Class != Refused ||
			!strings.Contains(c.Reason, "account identity") || strings.Contains(c.Reason, bad) {
			t.Errorf("identity %q: %s %q", bad, c.Class, c.Reason)
		}
	}
	c := addServer(tr, "comms", "mail", "https://mail.example/mcp", "", &AccountInput{Label: "work", Identity: "Alice Smith alice+work@corp.example"})
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if g.Account == nil || *g.Account != (AccountGrant{Label: "work", Identity: "Alice Smith alice+work@corp.example", IdentitySource: "agent"}) {
		t.Fatalf("grant account: %+v", g.Account)
	}
}

// The approved document and the rendered file of a connection added without
// an account are byte-identical to the fixtures captured before accounts
// existed (design §22.2.14 item 7), and the grant carries no account.
func TestAddMCPServer_NoAccountIsDefault(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.advert["https://mail.example/mcp"] = []string{"read_inbox", "send"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL_TOKEN"}, WriteTools: []string{"send"}})
	tr.mustClass(c, Widening)
	golden(t, "add_mcp_server.project.yaml", c.Ops[0].Content)
	golden(t, "add_mcp_server.rendered.json", string(c.Rendered))
	if c.Grant.Integrations[0].Account != nil {
		t.Fatalf("a default account with no identity must not appear in the grant: %+v", c.Grant.Integrations[0].Account)
	}
	tr.apply(c)
	// An explicit "default" with no identity is the same document.
	tr2 := newTree(t, "hermes")
	tr2.project("comms")
	tr2.advert["https://mail.example/mcp"] = []string{"read_inbox", "send"}
	c2 := tr2.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL_TOKEN"}, WriteTools: []string{"send"}, Account: &AccountInput{Label: "default"}})
	if c2.Ops[0].Content != c.Ops[0].Content || c2.Grant.Integrations[0].Account != nil {
		t.Fatalf("an explicit default differs from no account")
	}
	a := tr.render(VerbAddAPI, AddAPIInput{Project: "comms", Name: "bank", BaseURL: "https://bank.example", Methods: []string{"GET", "POST"}, Writes: true,
		Auth: APIAuthInput{Credential: "BANK_TOKEN", Prefix: "Bearer "}})
	tr.mustClass(a, Widening)
	golden(t, "add_api.project.yaml", a.Ops[0].Content)
	golden(t, "add_api.rendered.json", string(a.Rendered))
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile("testdata/accounts_golden/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s is not byte-identical to the pre-account fixture:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// A labelled connection renders its account on the entry (and its write
// sibling), and a later verb that re-renders the project keeps it.
func TestAccount_RenderedAndKeptByLaterVerbs(t *testing.T) {
	tr := accountTree(t)
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail-work", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL_WORK"}, WriteTools: []string{"send"}, Account: &AccountInput{Label: "work"}})
	tr.mustClass(c, Widening)
	if n := strings.Count(c.Ops[0].Content, `account: "work"`); n != 2 {
		t.Fatalf("the entry and its write sibling must both carry the account (%d):\n%s", n, c.Ops[0].Content)
	}
	tr.apply(c)
	a := tr.render(VerbAddAPI, AddAPIInput{Project: "comms", Name: "bank", BaseURL: "https://bank.example", Methods: []string{"GET"},
		Auth: APIAuthInput{Credential: "BANK"}, Account: &AccountInput{Label: "joint"}})
	tr.mustClass(a, Widening)
	tr.apply(a)
	tr.apply(tr.render(VerbSetBudget, SetBudgetInput{Project: "comms", MonthlyUSD: 1}))
	p := tr.state().Projects["hermes--comms"]
	for _, s := range p.Servers {
		if s.Account != "work" {
			t.Fatalf("server %s lost its account after set_budget: %+v", s.Name, s)
		}
	}
	if p.APIs[0].Account != "joint" {
		t.Fatalf("API lost its account after set_budget: %+v", p.APIs[0])
	}
}

// C1: a credential NAME belongs to one account across the namespace.
func TestCredential_NameBoundToOneAccount(t *testing.T) {
	tr := accountTree(t)
	tr.apply(addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"}))
	c := addServer(tr, "comms", "mail-personal", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "personal"})
	if c.Class != Refused || !strings.Contains(c.Reason, "the credential MAIL_WORK belongs to your work account (mail-work in hermes--comms); use another credential name for this account") {
		t.Fatalf("one name, two labels, one project: %s %q", c.Class, c.Reason)
	}
	c = addServer(tr, "home", "mail", "https://other.example/mcp", "MAIL_WORK", nil)
	if c.Class != Refused || !strings.Contains(c.Reason, "belongs to your work account (mail-work in hermes--comms)") {
		t.Fatalf("one name, two labels, two projects: %s %q", c.Class, c.Reason)
	}
	a := tr.render(VerbAddAPI, AddAPIInput{Project: "home", Name: "mailapi", BaseURL: "https://api.mail.example", Methods: []string{"GET"},
		Auth: APIAuthInput{Credential: "MAIL_WORK"}, Account: &AccountInput{Label: "home"}})
	if a.Class != Refused || !strings.Contains(a.Reason, "belongs to your work account") {
		t.Fatalf("an API naming another account's credential: %s %q", a.Class, a.Reason)
	}
	// The same label may share it (C4 says so).
	c = addServer(tr, "home", "mail", "https://other.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	tr.mustClass(c, Widening)
}

// C2: two OAuth servers of a project never share one sign-in slot (§22.1 M4).
func TestAddMCPServer_OAuthSlotNameCollisionRefused(t *testing.T) {
	tr := accountTree(t)
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "cal-work", URL: "https://cal.example/mcp", Auth: MCPAuthInput{Mode: "oauth"}}))
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "cal_work", URL: "https://cal2.example/mcp", Auth: MCPAuthInput{Mode: "oauth"}})
	if c.Class != Refused || !strings.Contains(c.Reason, `the server name "cal_work" would share the sign-in OAUTH_CAL_WORK with "cal-work"; choose another name`) {
		t.Fatalf("OAuth slot collision: %s %q", c.Class, c.Reason)
	}
	// A static server of that name does not use the slot.
	c = tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "cal_work", URL: "https://cal2.example/mcp", Auth: MCPAuthInput{Mode: "static", Credential: "CAL2"}})
	tr.mustClass(c, Widening)
	// Another project may use the same server name: slots are per project.
	c = tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "home", Name: "cal_work", URL: "https://cal2.example/mcp", Auth: MCPAuthInput{Mode: "oauth"}})
	tr.mustClass(c, Widening)
}

// C4: a credential another connection of the namespace already names is said.
func TestAddMCPServer_SharedCredentialIsSaid(t *testing.T) {
	tr := accountTree(t)
	first := addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	if strings.Contains(first.Sentence, "shares the credential") {
		t.Fatalf("a first use says it shares: %s", first.Sentence)
	}
	tr.apply(first)
	c := addServer(tr, "home", "mail", "https://other.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "It shares the credential MAIL_WORK with mail-work in hermes--comms: both act as the same account.") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	a := tr.render(VerbAddAPI, AddAPIInput{Project: "home", Name: "mailapi", BaseURL: "https://api.mail.example", Methods: []string{"GET"},
		Auth: APIAuthInput{Credential: "MAIL_WORK"}, Account: &AccountInput{Label: "work"}})
	tr.mustClass(a, Widening)
	if !strings.Contains(a.Sentence, "It shares the credential MAIL_WORK with mail-work in hermes--comms: both act as the same account.") {
		t.Fatalf("API sentence: %s", a.Sentence)
	}
}

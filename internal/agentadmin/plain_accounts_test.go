package agentadmin

import (
	"strings"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): every approval sentence and plain view
// names the account of each connection it concerns (agent-administered
// design §22.2.5, Review Focus 1: "two accounts at the same provider, and
// the approval says only the provider"). A default account alone at its host
// adds nothing, so every existing sentence reads as before.

func TestPlain_DefaultAccountSaysNothingNew(t *testing.T) {
	// The B0 measurement M6 sentence, recorded at the lane's base (§22.1.2).
	const m6 = `Your assistant (hermes) wants the project "hermes--comms" to run the workflow "hermes--comms--digest". It will return to your assistant: done (yes or no). It can reach the servers "mail-work" using hermes/MAIL_WORK.`
	tr := accountWorkflowTree(t, "")
	c := digestWF(tr, "")
	if c.Sentence != m6 {
		t.Fatalf("a default-account workflow's sentence changed:\n got %s\nwant %s", c.Sentence, m6)
	}
	if strings.Contains(c.Plain.Summary, "account") {
		t.Fatalf("a default-account plain summary names an account: %s", c.Plain.Summary)
	}
	add := addServer(accountTree(t), "comms", "mail", "https://mail.example/mcp", "MAIL", nil)
	if strings.Contains(add.Sentence, "account") || strings.Contains(add.Plain.Summary, "account") {
		t.Fatalf("a default add names an account: %s / %s", add.Sentence, add.Plain.Summary)
	}
}

func TestPlain_TwoAccountsSameHostBothNamed(t *testing.T) {
	tr := accountTree(t)
	first := addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"})
	if !strings.Contains(first.Sentence, "as your work account (not verified)") || !strings.Contains(first.Plain.Summary, "your work account (not verified)") {
		t.Fatalf("a labelled add: %s / %s", first.Sentence, first.Plain.Summary)
	}
	tr.apply(first)
	second := addServer(tr, "comms", "mail-personal", "https://mail.example/mcp", "MAIL_PERSONAL", nil)
	if !strings.Contains(second.Sentence, "as your default account (not verified)") || !strings.Contains(second.Plain.Summary, "your default account (not verified)") {
		t.Fatalf("a default account beside another at its host: %s / %s", second.Sentence, second.Plain.Summary)
	}
	tr.apply(second)
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "comms", Roles: []RoleInput{
		{Name: "reader", Instructions: "read", Tools: []string{"file_write", "mcp__mail-work__read_inbox", "mcp__mail-personal__read_inbox"}}}}))
	tr.approvals["hermes--comms"]["mail-work"] = withAccount(tr.approvals["hermes--comms"]["mail-work"], "alice@corp.example", IdentityFromProvider)
	w := tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "comms", Slug: "both", Purpose: "both",
		Steps: []StepInput{{Name: "read", Role: "reader", Instructions: "Read."}}, Egress: egress1()})
	for _, want := range []string{`"mail-work" as your work account (alice@corp.example)`, `"mail-personal" as your default account (not verified)`} {
		if !strings.Contains(w.Sentence, want) {
			t.Errorf("workflow sentence lacks %q: %s", want, w.Sentence)
		}
	}
	for _, want := range []string{"mail-work as your work account (alice@corp.example)", "mail-personal as your default account (not verified)"} {
		if !strings.Contains(w.Plain.Summary, want) {
			t.Errorf("workflow summary lacks %q: %s", want, w.Plain.Summary)
		}
	}
}

func TestPlain_AgentIdentityIsMarked(t *testing.T) {
	tr := accountTree(t)
	c := addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work", Identity: "alice@corp.example"})
	const want = "your work account (alice@corp.example as your assistant described it; not verified)"
	if !strings.Contains(c.Sentence, want) || !strings.Contains(c.Plain.Summary, want) {
		t.Fatalf("an agent-described identity is not marked: %s / %s", c.Sentence, c.Plain.Summary)
	}
	// A default label with an identity names it too.
	d := addServer(tr, "comms", "news", "https://news.example/mcp", "NEWS", &AccountInput{Label: "default", Identity: "Alice Smith"})
	if !strings.Contains(d.Sentence, "as your account (Alice Smith as your assistant described it; not verified)") {
		t.Fatalf("a default account with an identity: %s", d.Sentence)
	}
}

// C5: the credential sentence names the account and every connection of the
// namespace that uses the credential, and says when it replaces a stored key.
func TestCredentialSentence_NamesEveryUserAndAccount(t *testing.T) {
	tr := accountTree(t)
	tr.apply(addServer(tr, "comms", "mail-work", "https://mail.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"}))
	tr.apply(addServer(tr, "home", "mail", "https://other.example/mcp", "MAIL_WORK", &AccountInput{Label: "work"}))
	s := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "comms", Name: "MAIL_WORK", Purpose: "read mail", Kind: "secret"})
	tr.mustClass(s, Widening)
	for _, want := range []string{"mail-work at mail.example", "mail in hermes--home at other.example"} {
		if !strings.Contains(strings.Join(s.Slot.UsedBy, "; "), want) || !strings.Contains(s.Sentence, want) {
			t.Errorf("slot does not name %q: %v / %s", want, s.Slot.UsedBy, s.Sentence)
		}
	}
	if s.Slot.Account == nil || s.Slot.Account.Label != "work" {
		t.Fatalf("slot account: %+v", s.Slot.Account)
	}
	if !strings.Contains(s.Plain.Summary, "the key for your work account (not verified)") {
		t.Fatalf("summary: %s", s.Plain.Summary)
	}
	if strings.Contains(s.Sentence+s.Plain.Summary, "replaces the key") {
		t.Fatal("a first entry speaks of replacing")
	}
	// A rotation: the key is already stored.
	tr.credentialSet = map[string]bool{"MAIL_WORK": true}
	r := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "comms", Name: "MAIL_WORK", Purpose: "rotate", Kind: "secret"})
	const rot = "This replaces the key now stored for your work account; whichever account the new key belongs to is the one these connections will act as."
	if !strings.Contains(r.Sentence, rot) || !strings.Contains(r.Plain.Summary, rot) {
		t.Fatalf("rotation wording: %s / %s", r.Sentence, r.Plain.Summary)
	}
	// A credential of one project only keeps today's wording.
	tr2 := accountTree(t)
	tr2.apply(addServer(tr2, "comms", "mail", "https://mail.example/mcp", "MAIL", nil))
	one := tr2.render(VerbRequestCredential, RequestCredentialInput{Project: "comms", Name: "MAIL", Purpose: "read mail", Kind: "secret"})
	if one.Sentence != `Your assistant (hermes) asks you to add the credential MAIL for hermes--comms, used by mail at mail.example, for: "read mail". Enter it on this page; your assistant will never see it.` {
		t.Fatalf("a default single-project slot changed: %s", one.Sentence)
	}
}

// The tools approval after a sign-in names the account the server signed in
// as, with the provider's identity.
func TestPlain_ServerToolsNameTheAccount(t *testing.T) {
	tr := accountTree(t)
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "cal", URL: "https://cal.example/mcp",
		Auth: MCPAuthInput{Mode: "oauth"}, Account: &AccountInput{Label: "work"}}))
	tr.approvals["hermes--comms"]["cal"] = withAccount(tr.approvals["hermes--comms"]["cal"], "alice@corp.example", IdentityFromProvider)
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--comms", Server: "cal", Tools: []string{"list_events"}})
	tr.mustClass(c, Widening)
	const want = "cal at cal.example as your work account (alice@corp.example) is now connected"
	if !strings.Contains(c.Sentence, want) || !strings.Contains(c.Plain.Summary, "as your work account (alice@corp.example)") {
		t.Fatalf("tools approval: %s / %s", c.Sentence, c.Plain.Summary)
	}
}

func withAccount(a IntegrationApproval, identity, source string) IntegrationApproval {
	a.AccountIdentity, a.IdentitySource = identity, source
	return a
}

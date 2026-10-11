package agentadmin

import (
	"strings"
	"testing"
)

// Security scan of lane B (2026-10-09, GitHub #119 item 8): the findings
// reproduced before their fixes.

// Incident 1 (account binding in install_recipe): an entry reused an
// existing server of the same name, URL and credential without checking
// which account it acts as. A static credential is caught by C1 (one name,
// one label), but an OAuth entry has no credential name: a hand-added OAuth
// "calendar-work" labelled "personal" was reused by an install "for your
// work account", so the device approved a sentence naming the work account
// while the install bound the personal connection (approval mismatch, F2).
func TestInstallRecipe_RefusesReusingAnotherAccountsServer(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "personal", Name: "calendar-work", URL: "https://cal-work.example/mcp",
		Auth: MCPAuthInput{Mode: "oauth"}, Account: &AccountInput{Label: "personal"}}))
	st := tr.state()
	p := st.Projects["hermes--personal"]
	need := RecipeNeed{Name: "calendar-work", URL: "{{calendar_server_url}}", ReadTools: []string{"calendar_list_events"},
		Credential: &RecipeCredential{Name: OAuthCredentialName("calendar-work"), Kind: CredentialOAuth}}
	_, why := classifyEntry(st, p, need, "https://cal-work.example/mcp", "work")
	if !strings.Contains(why, `already has a server named "calendar-work" that acts as your personal account`) {
		t.Fatalf("an install for the work account reused the personal account's server: %q", why)
	}
	// The same label is reused as before.
	if _, why := classifyEntry(st, p, need, "https://cal-work.example/mcp", "personal"); why != "" {
		t.Fatalf("the same account's server was refused: %q", why)
	}
}

// Incident 1, the sign-in slot: a recipe's new OAuth server could share its
// slot with a hand-added one ("calendar-work" against "calendar_work"), so a
// sign-in the person made for one account could serve the other connection;
// C2 was applied in add_mcp_server only.
func TestInstallRecipe_OAuthSlotCollisionRefused(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "personal", Name: "calendar_work", URL: "https://x.example/mcp",
		Auth: MCPAuthInput{Mode: "oauth"}, Account: &AccountInput{Label: "personal"}}))
	st := tr.state()
	need := RecipeNeed{Name: "calendar-work", URL: "{{calendar_server_url}}", ReadTools: []string{"calendar_list_events"},
		Credential: &RecipeCredential{Name: OAuthCredentialName("calendar-work"), Kind: CredentialOAuth}}
	_, why := classifyEntry(st, st.Projects["hermes--personal"], need, "https://cal-work.example/mcp", "work")
	if !strings.Contains(why, "would share the sign-in OAUTH_CALENDAR_WORK") {
		t.Fatalf("a recipe server sharing a sign-in slot was accepted: %q", why)
	}
}

func TestInstallRecipe_RefusesDifferentDefaultCredentialAtSharedHost(t *testing.T) {
	tr := shippedTree(t)
	tr.approveServer("personal", "calendar-manual", "https://cal.example/mcp")

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", ""))
	if c.Class != Refused || !strings.Contains(c.Reason, "account") || !strings.Contains(c.Reason, "install_recipe") {
		t.Fatalf("default recipe entry at a conflicting host: %s %q", c.Class, c.Reason)
	}
}

func TestInstallRecipe_DistinctAccountAllowsSharedHost(t *testing.T) {
	tr := shippedTree(t)
	tr.approveServer("personal", "calendar-manual", "https://cal.example/mcp")

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", "work"))
	tr.mustClass(c, Widening)
}

func TestInstallRecipe_SharedCredentialKeepsItsDefaultAccount(t *testing.T) {
	tr := shippedTree(t)
	manual := tr.render(VerbAddMCPServer, AddMCPServerInput{
		Project: "personal", Name: "calendar-manual", URL: "https://cal.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "CALENDAR_TOKEN"},
	})
	tr.mustClass(manual, Widening)
	tr.apply(manual)

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", ""))
	tr.mustClass(c, Widening)
	project := opContent(c, "projects/hermes--personal.yaml")
	if !strings.Contains(project, `name: "calendar"`) || strings.Contains(project, "account:") {
		t.Fatalf("shared-credential default install changed its account rendering:\n%s", project)
	}
}

func TestInstallRecipe_NewEntriesCheckSharedHostAgainstEarlierNeeds(t *testing.T) {
	tr := shippedTree(t)
	// shippedTree creates a fresh renderer and recipe catalogue per test, so
	// this malformed needs pair cannot leak into another test's recipe source.
	rec := tr.r.cat.Get("agenda")
	rec.Needs = append(rec.Needs, RecipeNeed{
		Name: "calendar-alt", URL: "https://cal.example/mcp", ReadTools: []string{"calendar_list_events"},
		Credential: &RecipeCredential{Name: "CALENDAR_ALT_TOKEN", Kind: CredentialSecret},
	})

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", ""))
	if c.Class != Refused || !strings.Contains(c.Reason, "account") || !strings.Contains(c.Reason, "install_recipe") {
		t.Fatalf("recipe's second same-host credential: %s %q", c.Class, c.Reason)
	}
}

// Lane B review aae3 finding 4: role ownership picks the longest prefix
// among a recipe's installs, so a label that begins a team role's own name
// could make a role of one install read as another's. agenda's team has a
// role "reader"; the label "reader" would render "agenda-reader-reader" and
// claim the catalogue install's "agenda-reader".
func TestInstallRecipe_LabelThatShadowsARoleRefused(t *testing.T) {
	tr := shippedTree(t)
	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", "reader"))
	if c.Class != Refused || !strings.Contains(c.Reason, `is also the start of the recipe's role "reader"`) {
		t.Fatalf("a label shadowing a role: %s %q", c.Class, c.Reason)
	}
}

// Lane B implementation review 289c finding 4: an agent-described identity
// with runs of spaces ("alice   smith") reads like another address in the
// sentence; one space between words only.
func TestAccountIdentity_NoSpaceRuns(t *testing.T) {
	tr := accountTree(t)
	if c := addServer(tr, "comms", "mail", "https://mail.example/mcp", "", &AccountInput{Label: "work", Identity: "alice  smith"}); c.Class != Refused {
		t.Fatalf("an identity with a run of spaces: %s", c.Class)
	}
}

// Incident 2 (a recipe acting with a secret it did not create): installing a
// recipe into a second project bound the credential the first project's
// install had entered (CALENDAR_TOKEN, §22.1 M7b), and the sentence said
// only "you enter any that is not set yet": the person approved, without
// being told, a new server at another URL acting with the first project's
// key. The sentence now says the key is shared (C4).
func TestInstallRecipe_SharedCredentialIsSaid(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "work", Purpose: "Work"}))
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", "")))
	in := agendaFor("https://cal-other.example/mcp", "")
	in.Project = "work"
	c := tr.render(VerbInstallRecipe, in)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "It shares the credential CALENDAR_TOKEN with calendar in hermes--personal: both act as the same account.") {
		t.Fatalf("a second project's install does not say it shares the key: %s", c.Sentence)
	}
}

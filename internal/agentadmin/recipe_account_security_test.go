package agentadmin

import (
	"encoding/json"
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

// GitHub #402 (2026-10-16, design §22.2.17), inverting GitHub #316's
// refusal: a recipe entry joining a host that a hand-added connection
// reaches under the same label is the same account, said on the device
// with how to decline, not refused. The other direction is kept: a
// hand-added connection with a new credential at a host a recipe entry
// reaches is refused (the agent chose that name).
func TestInstallRecipe_JoinsHandAddedHostButHandAddedStillRefused(t *testing.T) {
	tr := shippedTree(t)
	tr.approveServer("personal", "calendar-manual", "https://cal.example/mcp")

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp/v2", ""))
	tr.mustClass(c, Widening)
	want := " calendar reaches cal.example, which calendar-manual already reaches as your default account (not verified): it will act as that same account, with its own credential. If that is another account, decline this and have your assistant install it with another account label."
	if !strings.Contains(c.Sentence, want) {
		t.Fatalf("joining a hand-added connection's host is not said:\n%s", c.Sentence)
	}
	if got := c.accounts["calendar"]; got != "your default account (not verified)" {
		t.Fatalf("the plain view does not name the joining entry's account: %q", got)
	}
	tr.apply(c)

	manual := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "personal", Name: "tasks", URL: "https://cal.example/tasks",
		Auth: MCPAuthInput{Mode: "static", Credential: "TASKS_TOKEN"}})
	if manual.Class != Refused || !strings.Contains(manual.Reason, "give this one another account label") {
		t.Fatalf("a hand-added new credential next to a recipe entry: %s %q", manual.Class, manual.Reason)
	}
}

// GitHub #402 review 4d33 finding 1: the disclosure is said exactly where
// the removed refusal fired, so an entry with no credential (no account,
// the refusal's early return) is never said.
func TestInstallRecipe_NoCredentialEntryIsNotSaid(t *testing.T) {
	tr := shippedTree(t)
	// shippedTree builds a fresh catalogue per test, so this edit stays here.
	tr.r.cat.Get("agenda").Needs[0].Credential = nil
	tr.approveServer("personal", "calendar-manual", "https://cal.example/mcp")

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp/v2", ""))
	tr.mustClass(c, Widening)
	if strings.Contains(c.Sentence, "same account") || strings.Contains(c.Sentence, "one account") {
		t.Fatalf("a no-credential entry was said as an account:\n%s", c.Sentence)
	}
	if _, ok := c.accounts["calendar"]; ok {
		t.Fatalf("the plain view names an account for a no-credential entry: %v", c.accounts)
	}
}

// GitHub #402 code review 7142 finding 1: the planning copy planInstall
// keeps is what lets C2 (and the name lookup) see the install's earlier new
// entries; #402 dropped it with the two-connections call, so two new OAuth
// servers of one install sharing a sign-in slot were accepted.
func TestInstallRecipe_NewOAuthEntriesShareNoSlot(t *testing.T) {
	tr := shippedTree(t)
	rec := tr.r.cat.Get("agenda")
	rec.Needs[0].Name, rec.Needs[0].Credential = "cal-work", &RecipeCredential{Name: OAuthCredentialName("cal-work"), Kind: CredentialOAuth}
	rec.Needs = append(rec.Needs, RecipeNeed{
		Name: "cal_work", URL: "https://other.example/mcp", ReadTools: []string{"calendar_list_events"},
		Credential: &RecipeCredential{Name: OAuthCredentialName("cal_work"), Kind: CredentialOAuth},
	})
	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", ""))
	if c.Class != Refused || !strings.Contains(c.Reason, "would share the sign-in OAUTH_CAL_WORK") {
		t.Fatalf("two new OAuth servers of one install share a sign-in slot: %s %q", c.Class, c.Reason)
	}
}

// GitHub #402 code review 7142 finding 3: when two needs of one install at
// a host share a credential, "each with its own credential" would be
// false; the sentence counts the credentials instead.
func TestInstallRecipe_OneInstallSharedKeyIsCounted(t *testing.T) {
	tr := shippedTree(t)
	rec := tr.r.cat.Get("morning-brief")
	rec.Needs = append(rec.Needs, RecipeNeed{
		Name: "calendar2", URL: "https://gws.example/calendar2/mcp", ReadTools: []string{"calendar_list_events"},
		Credential: &RecipeCredential{Name: "CALENDAR_TOKEN", Kind: CredentialSecret},
	})
	in := shippedVars("morning-brief")
	in.Project = "personal"
	in.Variables["mail_server_url"] = json.RawMessage(`"https://gws.example/mail/mcp"`)
	in.Variables["calendar_server_url"] = json.RawMessage(`"https://gws.example/calendar/mcp"`)
	c := tr.render(VerbInstallRecipe, in)
	tr.mustClass(c, Widening)
	if want := " mail, calendar and calendar2 reach gws.example as one account, your default account (not verified), with 2 credentials among them."; !strings.Contains(c.Sentence, want) {
		t.Fatalf("a shared key within one install:\n%s\nwant %q", c.Sentence, want)
	}
}

// GitHub #402 review 5ccb finding 4: the joined connection is named by its
// own account words, so a provider-identified account is called by its
// identity, never also "not verified".
func TestInstallRecipe_JoinedConnectionKeepsItsIdentity(t *testing.T) {
	tr := shippedTree(t)
	tr.approveServer("personal", "calendar-manual", "https://cal.example/mcp")
	tr.approvals["hermes--personal"]["calendar-manual"] = withAccount(tr.approvals["hermes--personal"]["calendar-manual"], "alice@corp.example", IdentityFromProvider)

	c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp/v2", ""))
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "which calendar-manual already reaches as your default account (alice@corp.example): it will act as that same account") {
		t.Fatalf("the joined connection is not named by its identity:\n%s", c.Sentence)
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

// GitHub #402 (2026-10-16, design §22.2.17), inverting GitHub #316's
// intra-install refusal: morning-brief with mail and calendar on one host
// was refused against its own first need. The needs of one install are one
// account by construction (one label, 22.2.8); the sentence says so, with
// declining as the recourse, under a non-default label too.
func TestInstallRecipe_OneInstallIsOneAccount(t *testing.T) {
	for _, label := range []string{"", "work"} {
		tr := shippedTree(t)
		in := shippedVars("morning-brief")
		in.Project = "personal"
		in.Account = label
		in.Variables["mail_server_url"] = json.RawMessage(`"https://gws.example/mail/mcp"`)
		in.Variables["calendar_server_url"] = json.RawMessage(`"https://gws.example/calendar/mcp"`)
		c := tr.render(VerbInstallRecipe, in)
		tr.mustClass(c, Widening)
		mail, cal, phrase := "mail", "calendar", "your default account (not verified)"
		if label != "" {
			mail, cal, phrase = "mail-work", "calendar-work", "your work account (not verified)"
		}
		want := " " + mail + " and " + cal + " reach gws.example as one account, " + phrase + ", each with its own credential. If they are not one account, decline this."
		if !strings.Contains(c.Sentence, want) {
			t.Fatalf("label %q: the sentence does not say the needs are one account:\n%s\nwant %q", label, c.Sentence, want)
		}
		if c.accounts[cal] != phrase || c.accounts[mail] != phrase {
			t.Fatalf("label %q: the plain view does not name the account: %v", label, c.accounts)
		}
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

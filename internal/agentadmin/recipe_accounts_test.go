package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): a recipe may be installed once per
// account (agent-administered design §22.2.8). install_recipe takes one
// account label for the whole install; a non-default label suffixes every
// name the install renders, so two accounts never share a server, a
// credential, a workflow or a role, and the default install renders exactly
// as before.

func agendaFor(url, account string) InstallRecipeInput {
	in := shippedVars("agenda")
	in.Project = "personal"
	in.Variables["calendar_server_url"] = json.RawMessage(`"` + url + `"`)
	in.Account = account
	return in
}

func opContent(c Change, path string) string {
	for _, op := range c.Ops {
		if op.Path == path {
			return op.Content
		}
	}
	return ""
}

func TestInstallRecipe_DefaultAccountUnchanged(t *testing.T) {
	for _, account := range []string{"", "default"} {
		tr := shippedTree(t)
		c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", account))
		tr.mustClass(c, Widening)
		for path, fixture := range map[string]string{"projects/hermes--personal.yaml": "recipe_agenda.project.yaml",
			"swarms/hermes--personal.md": "recipe_agenda.swarm.md", "workflows/hermes--personal--agenda.md": "recipe_agenda.workflow.md"} {
			golden(t, fixture, opContent(c, path))
		}
		golden(t, "recipe_agenda.sentence.txt", c.Sentence)
		if c.Grant.Integrations[0].Account != nil {
			t.Fatalf("account %q: a default install's grant names an account", account)
		}
	}
}

func TestInstallRecipe_SecondAccountGetsSuffixedNames(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", "")))
	c := tr.render(VerbInstallRecipe, agendaFor("https://cal-work.example/mcp", "work"))
	tr.mustClass(c, Widening)
	project := opContent(c, "projects/hermes--personal.yaml")
	swarm := opContent(c, "swarms/hermes--personal.md")
	wf := opContent(c, "workflows/hermes--personal--agenda-work.md")
	for what, want := range map[string]string{
		"server":         `name: "calendar-work"`,
		"account":        `account: "work"`,
		"credential":     `secret://hermes/CALENDAR_TOKEN_WORK`,
		"default kept":   `name: "calendar"`,
		"role":           "agenda-work-reader",
		"role tool":      "mcp__calendar-work__calendar_list_events",
		"workflow":       "hermes--personal--agenda-work",
		"step role":      "agenda-work-reader",
		"egress source":  `"calendar-work"`,
		"step tool text": "mcp__calendar-work__calendar_list_events",
	} {
		if !strings.Contains(project+swarm+wf, want) {
			t.Errorf("%s: the install lacks %q", what, want)
		}
	}
	if !strings.Contains(wf, "agenda-work-reader") {
		t.Fatalf("the work workflow's step does not run the work role:\n%s", wf)
	}
	if strings.Contains(wf, "mcp__calendar__") {
		t.Fatalf("the work workflow still names the default server:\n%s", wf)
	}
	if len(c.Credentials) != 1 || c.Credentials[0].Name != "CALENDAR_TOKEN_WORK" {
		t.Fatalf("credential follow-ups %+v", c.Credentials)
	}
	g := c.Grant.Integrations[0]
	if g.Name != "calendar-work" || g.Account == nil || g.Account.Label != "work" {
		t.Fatalf("grant %+v", g)
	}
	if !strings.Contains(c.Sentence, "for your work account (not verified)") || !strings.Contains(c.Sentence, "CALENDAR_TOKEN_WORK") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	if !strings.Contains(c.Plain.Summary, "calendar-work (cal-work.example) as your work account (not verified)") {
		t.Fatalf("summary: %s", c.Plain.Summary)
	}
	tr.apply(c)
	// The reach binds the account (B4) and the default install is untouched.
	if w := tr.state().Workflows["hermes--personal--agenda-work"]; w == nil {
		t.Fatal("the work install did not load")
	}
	if w := tr.state().Workflows["hermes--personal--agenda"]; w == nil {
		t.Fatal("the default install was removed")
	}
}

func TestInstallRecipe_SameAccountTwiceRefused(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal-work.example/mcp", "work")))
	// The same account again at another URL: refused, as a second default
	// install is today.
	c := tr.render(VerbInstallRecipe, agendaFor("https://other.example/mcp", "work"))
	if c.Class != Refused || !strings.Contains(c.Reason, `already has a server named "calendar-work"`) {
		t.Fatalf("a second work install: %s %q", c.Class, c.Reason)
	}
	// The same account and URL again: a reinstall, nothing to do.
	if again := tr.render(VerbInstallRecipe, agendaFor("https://cal-work.example/mcp", "work")); again.Class != Inert || len(again.Ops) != 0 {
		t.Fatalf("reinstalling the same account: %s, %d ops", again.Class, len(again.Ops))
	}
}

// A role belongs to exactly one install: a default install never claims the
// work install's roles, and the other way round.
func TestInstallRecipe_OwnershipIsExact(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", "")))
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal-work.example/mcp", "work")))
	// Reinstalling the default with another schedule re-renders its roles.
	in := agendaFor("https://cal.example/mcp", "")
	in.Schedule = json.RawMessage(`null`)
	tr.apply(tr.render(VerbInstallRecipe, in))
	roles := tr.state().Projects["hermes--personal"].Swarm.Roles
	var names []string
	for _, r := range roles {
		names = append(names, r.Name)
	}
	if !contains(names, "agenda-reader") || !contains(names, "agenda-work-reader") {
		t.Fatalf("roles after reinstalling the default: %v", names)
	}
}

func TestInstallRecipe_AccountLabelChecked(t *testing.T) {
	tr := shippedTree(t)
	for _, bad := range []string{"Work", "a b", strings.Repeat("a", 25)} {
		if c := tr.render(VerbInstallRecipe, agendaFor("https://cal.example/mcp", bad)); c.Class != Refused {
			t.Errorf("label %q accepted", bad)
		}
	}
}

func TestListRecipes_SaysOncePerAccount(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, agendaFor("https://cal-work.example/mcp", "work")))
	for _, v := range tr.r.ListRecipes(tr.state()) {
		if !strings.Contains(v.Accounts, "once per account") {
			t.Fatalf("%s: %q", v.Name, v.Accounts)
		}
		if v.Name == "agenda" && (len(v.Installed) != 1 || v.Installed[0].Account != "work") {
			t.Fatalf("installed: %+v", v.Installed)
		}
	}
}

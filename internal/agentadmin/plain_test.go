package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// Design §18.7 (operator, 2026-10-02): an approval a non-engineer can judge.
// The daemon, never an LLM or the agent, writes a plain summary and a
// Low/Medium/High level with reasons, from the request's typed facts.

func plainState() *State {
	return &State{Namespace: "hermes", CeilingUSD: 10}
}

// Every verb that can reach the phone has its own phrase and a level.
func TestExplain_EveryApprovableVerbHasAPhraseAndALevel(t *testing.T) {
	st := plainState()
	cases := map[string]Change{
		VerbCreateProject:      {Verb: VerbCreateProject, Class: Widening, Grant: Grant{AddsUSD: f64(2), MaxTotalUSD: f64(12)}, Locks: []string{"project:hermes--finance"}},
		VerbSetBudget:          {Verb: VerbSetBudget, Class: Widening, Grant: Grant{AddsUSD: f64(3)}, Locks: []string{"project:hermes--finance"}},
		VerbDefineWorkflow:     {Verb: VerbDefineWorkflow, Class: Widening, Grant: Grant{Workflows: map[string]string{"hermes--finance--spend": "h"}}, reach: &ReachSignature{}},
		VerbDefineSwarm:        {Verb: VerbDefineSwarm, Class: Widening, Grant: Grant{Workflows: map[string]string{"hermes--finance--spend": "h"}}, Locks: []string{"project:hermes--finance"}},
		VerbAddMCPServer:       {Verb: VerbAddMCPServer, Class: Widening, Grant: Grant{Integrations: []IntegrationGrant{{Project: "hermes--finance", Name: "fio", Kind: "mcp", URL: "https://api.fio.example/mcp"}}}},
		VerbAddAPI:             {Verb: VerbAddAPI, Class: Widening, Grant: Grant{Integrations: []IntegrationGrant{{Project: "hermes--finance", Name: "gh", Kind: "api", URL: "https://api.github.com"}}}},
		VerbRequestCredential:  {Verb: VerbRequestCredential, Slot: &CredentialSlot{Namespace: "hermes", Project: "hermes--finance", Name: "FIO_TOKEN", Kind: CredentialSecret, UsedBy: []string{"fio at api.fio.example"}}},
		VerbApproveServerTools: {Verb: VerbApproveServerTools, Class: Widening, Grant: Grant{Integrations: []IntegrationGrant{{Project: "hermes--finance", Name: "fio", Kind: "mcp", URL: "https://api.fio.example/mcp", Read: []string{"balance"}}}}},
	}
	seen := map[string]bool{}
	for verb, c := range cases {
		p := explain(st, &c)
		if p == nil || p.Summary == "" || p.Level == "" || len(p.Reasons) == 0 {
			t.Errorf("%s: no plain view: %+v", verb, p)
			continue
		}
		if p.Summary == genericSummary {
			t.Errorf("%s: falls back to the generic phrase", verb)
		}
		if seen[p.Summary] {
			t.Errorf("%s: shares its phrase with another verb", verb)
		}
		seen[p.Summary] = true
	}
}

// Hermes approval transport design §4.1 (§18.7 row): a host action has its
// own phrase and is always High, because Hermes's rules flagged the command
// as dangerous by definition; Vornik does not re-judge them from the phone.
func TestExplainHostAction_PhraseAndLevel(t *testing.T) {
	p := ExplainHostAction()
	if p.Summary != "Hermes wants to run a command on its own computer that its safety rules flagged. Vornik cannot stop it; Hermes will run it only if you allow it." {
		t.Fatalf("summary %q", p.Summary)
	}
	if p.Level != LevelHigh || len(p.Reasons) == 0 || p.Summary == genericSummary {
		t.Fatalf("view %+v", p)
	}
}

// One test per row of the rule table; the highest matching rule wins.
func TestExplain_RiskTable(t *testing.T) {
	st := plainState()
	level := func(c Change) (string, []string) {
		p := explain(st, &c)
		return p.Level, p.Reasons
	}
	has := func(rs []string, s string) bool {
		for _, r := range rs {
			if strings.Contains(r, s) {
				return true
			}
		}
		return false
	}
	// High: a connection with writes.
	if l, r := level(Change{Verb: VerbAddMCPServer, Class: Widening, Grant: Grant{Integrations: []IntegrationGrant{{Name: "mail", Kind: "mcp", URL: "https://m.example/mcp", Write: []string{"send"}}}}}); l != LevelHigh || !has(r, "suggest changes") {
		t.Errorf("writes: %s %v", l, r)
	}
	// High: a workflow that proposes writes.
	if l, _ := level(Change{Verb: VerbDefineWorkflow, Class: Widening, reach: &ReachSignature{Proposes: json.RawMessage(`[{"action":"reply"}]`)}}); l != LevelHigh {
		t.Errorf("proposes: %s", l)
	}
	// High, with its own reason and summary: a proposal that declares
	// standing (broker write-actions design, tier 2 revised: say the
	// concession, never "each approved by you" alone).
	standing := Change{Verb: VerbDefineWorkflow, Class: Widening, reach: &ReachSignature{Proposes: json.RawMessage(`[{"action":"reply","standing":{"key":["to"]}}]`)}}
	if l, r := level(standing); l != LevelHigh || !has(r, "without showing you their text") {
		t.Errorf("standing: %s %v", l, r)
	}
	if s := explain(st, &standing).Summary; strings.Contains(s, "nothing changes unless you approve each one") || !strings.Contains(s, "without showing you their text") ||
		strings.Contains(s, " like ") || strings.Contains(s, "similar") {
		t.Errorf("standing summary: %s", s)
	}
	// High: the ceiling to more than twice its value, or above $100.
	if l, r := level(Change{Verb: VerbSetBudget, Class: Widening, Grant: Grant{AddsUSD: f64(15), MaxTotalUSD: f64(25)}}); l != LevelHigh || !has(r, "$25") {
		t.Errorf("ceiling x2: %s %v", l, r)
	}
	if l, _ := level(Change{Verb: VerbSetBudget, Class: Widening, Grant: Grant{AddsUSD: f64(1), MaxTotalUSD: f64(101)}}); l != LevelHigh {
		t.Errorf("ceiling over 100: %s", l)
	}
	// Medium: a read-only connection; a credential; a schedule; a budget raise.
	if l, r := level(Change{Verb: VerbAddAPI, Class: Widening, Grant: Grant{Integrations: []IntegrationGrant{{Name: "gh", Kind: "api", URL: "https://api.github.com"}}}}); l != LevelMedium || !has(r, "api.github.com") {
		t.Errorf("read-only: %s %v", l, r)
	}
	if l, _ := level(Change{Verb: VerbRequestCredential, Slot: &CredentialSlot{Name: "T", Kind: CredentialSecret}}); l != LevelMedium {
		t.Errorf("credential: %s", l)
	}
	if l, r := level(Change{Verb: VerbDefineWorkflow, Class: Widening, reach: &ReachSignature{Schedule: `{"cron":"0 8 1 * *","timezone":"Europe/Prague"}`}}); l != LevelMedium || !has(r, "by itself") {
		t.Errorf("schedule: %s %v", l, r)
	}
	if l, _ := level(Change{Verb: VerbSetBudget, Class: Widening, Grant: Grant{AddsUSD: f64(2)}}); l != LevelMedium {
		t.Errorf("raise: %s", l)
	}
	// Medium: task content sent to a remote model provider (design §18.6
	// item 2), named by sub-provider and host, with the remote-model phrase.
	remote := Change{Verb: VerbDefineSwarm, Class: Widening, Locks: []string{"project:hermes--finance"},
		Grant: Grant{Models: []ModelGrant{{Destination: "vertex@aiplatform.googleapis.com", Model: "google/gemini-pro", Role: "critic"}}}}
	if l, r := level(remote); l != LevelMedium || !has(r, "critic role works on to vertex at aiplatform.googleapis.com") {
		t.Errorf("remote model: %s %v", l, r)
	}
	if s := explain(st, &remote).Summary; !strings.Contains(s, "use a model at vertex at aiplatform.googleapis.com") {
		t.Errorf("remote model summary: %s", s)
	}
	// Low: a workflow that reaches nothing and spends within the ceiling.
	if l, r := level(Change{Verb: VerbDefineWorkflow, Class: Widening, reach: &ReachSignature{}}); l != LevelLow || len(r) == 0 {
		t.Errorf("nothing: %s %v", l, r)
	}
}

// The view is part of what the device approves (§9.1): it rides in the
// canonical rendered document.
func TestRender_PlainViewIsInTheApprovedDocument(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{{Name: "reader", Instructions: "Read.", Tools: []string{"file_write"}}}}))
	c := defineWF(tr, egress1(), "reader")
	var doc struct {
		Plain *PlainView `json:"plain"`
	}
	if err := json.Unmarshal(c.Rendered, &doc); err != nil || doc.Plain == nil || doc.Plain.Level != LevelLow {
		t.Fatalf("rendered plain view: %+v (%v)\n%s", doc.Plain, err, c.Rendered)
	}
}

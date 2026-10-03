package agentadmin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The risk levels of design §18.7, shown as a word and a colour, never a
// colour alone.
const (
	LevelLow    = "Low"
	LevelMedium = "Medium"
	LevelHigh   = "High"
)

// PlainView is what a person approving on a phone reads first (design
// §18.7): a summary from a fixed phrasebook keyed on the request's verb and
// facts, and a level with its reasons from a fixed rule table. Nothing in it
// is written by an LLM or by the requesting agent (§9.1); it rides in the
// canonical document the device approves.
type PlainView struct {
	Summary string   `json:"summary"`
	Level   string   `json:"level"`
	Reasons []string `json:"reasons"`
}

// genericSummary is never expected; a test fails on any approvable verb
// that falls back to it.
const genericSummary = "Your assistant asks to change its setup in Vornik."

// highCeilingUSD is the absolute monthly limit above which a raise is High.
const highCeilingUSD = 100

// explain builds the plain view of a change that goes to the phone: a
// widening change or a credential slot. Inert and refused changes get none.
func explain(st *State, c *Change) *PlainView {
	if c.Slot == nil && c.Class != Widening {
		return nil
	}
	p := &PlainView{Summary: summaryOf(c)}
	level, reasons := riskOf(st, c)
	p.Level, p.Reasons = level, reasons
	return p
}

func summaryOf(c *Change) string {
	switch c.Verb {
	case VerbRequestCredential:
		if c.Slot == nil {
			return genericSummary
		}
		if c.Slot.Kind == CredentialOAuth {
			return fmt.Sprintf("You are asked to sign in to %s for Vornik. Your assistant never sees your login.", c.Slot.Server)
		}
		s := fmt.Sprintf("You are asked to give Vornik the %s key", c.Slot.Name)
		if len(c.Slot.UsedBy) > 0 {
			s += ", used by " + strings.Join(c.Slot.UsedBy, " and ")
		}
		return s + ". Your assistant never sees it."
	case VerbAddMCPServer, VerbAddAPI:
		g := firstIntegration(c)
		s := fmt.Sprintf("Your assistant wants to read from %s (%s). It can look at information there but cannot change anything.", g.Name, hostOf(g.URL))
		if len(g.Write) > 0 {
			s += " It may also suggest changes there; nothing changes unless you approve each one."
		}
		return s
	case VerbApproveServerTools:
		g := firstIntegration(c)
		return fmt.Sprintf("Vornik listed what %s (%s) offers. Approving lets your assistant's skills read through: %s.",
			g.Name, hostOf(g.URL), strings.Join(g.Read, ", "))
	case VerbDefineWorkflow:
		return workflowSummary(c)
	case VerbDefineSwarm:
		if len(c.Grant.Models) > 0 {
			return remoteModelSummary(c)
		}
		return fmt.Sprintf("Your assistant wants to change what its team in '%s' can use. Some of its skills will reach more than before.", projectOfLocks(c))
	case VerbInstallRecipe:
		return recipeSummary(c)
	case VerbCreateProject:
		return fmt.Sprintf("Your assistant wants a new project, '%s'. %s", projectOfLocks(c), spendPhrase(c))
	case VerbSetBudget:
		return fmt.Sprintf("Your assistant wants a bigger budget for '%s'. %s", projectOfLocks(c), spendPhrase(c))
	}
	return genericSummary
}

// remoteModelSummary is §18.7's remote-model phrase (design §18.6 item 2):
// a team member on a model at a destination the namespace has not used,
// named by sub-provider and host.
func remoteModelSummary(c *Change) string {
	var dests []string
	for _, m := range c.Grant.Models {
		dests = append(dests, destinationWords(m.Destination))
	}
	where := strings.Join(dests, " and ")
	s := fmt.Sprintf("Your assistant wants a team member in '%s' to use a model at %s. What that member works on is sent to %s.", projectOfLocks(c), where, where)
	if len(c.Grant.Workflows) > 0 {
		s += " Some of its skills will reach more than before."
	}
	return s
}

// destinationWords turns "vertex@aiplatform.googleapis.com" into "vertex at
// aiplatform.googleapis.com".
func destinationWords(dest string) string {
	sub, host, ok := strings.Cut(dest, "@")
	if !ok {
		return dest
	}
	return sub + " at " + host
}

func workflowSummary(c *Change) string {
	wf := ""
	for id := range c.Grant.Workflows {
		wf = id
	}
	s := "Your assistant wants to add a skill"
	if wf != "" {
		s += fmt.Sprintf(", '%s'", wf)
	}
	s += "."
	if c.reach == nil || len(c.reach.Integrations) == 0 {
		s += " It can only work with what you or your assistant give it; it cannot reach the internet or your accounts."
	} else {
		s += " It can read from " + strings.Join(c.reach.Integrations, ", ") + "."
	}
	// Broker design §18.4: the line saying what the agent can hand the team.
	if c.reach != nil && len(c.reach.Documents) > 0 {
		s += " Your assistant can give it " + documentPhrase(c.reach.Documents) + " to work on."
	}
	if c.reach != nil && proposesSomething(c.reach.Proposes) {
		if proposesStanding(c.reach.Proposes) {
			s += " It may suggest changes there; you approve each one, or, for writes to an address you approved, you may let future ones be sent without showing you their text."
		} else {
			s += " It may suggest changes there; nothing changes unless you approve each one."
		}
	}
	if c.reach != nil && c.reach.Schedule != "" {
		s += " It will run by itself " + scheduleWords(c.reach.Schedule) + "."
	}
	return s
}

// recipeSummary says a recipe install: a ready-made, tested skill, what it
// reads from and what it returns, in the catalogue's own words.
func recipeSummary(c *Change) string {
	if c.recipe == nil {
		return genericSummary
	}
	s := fmt.Sprintf("Your assistant wants to install '%s', a ready-made skill Vornik ships and tests.", c.recipe.Title)
	var reads []string
	for _, g := range c.Grant.Integrations {
		reads = append(reads, fmt.Sprintf("%s (%s)", g.Name, hostOf(g.URL)))
	}
	if len(reads) == 0 && c.reach != nil {
		reads = c.reach.Integrations
	}
	if len(reads) > 0 {
		s += " It can read from " + strings.Join(reads, " and ") + " but cannot change anything there."
	}
	s += " It returns " + c.recipe.Returns + "."
	if c.reach != nil && c.reach.Schedule != "" {
		s += " It will run by itself " + scheduleWords(c.reach.Schedule) + "."
	}
	return s
}

// riskOf applies the rule table; the highest matching rule wins, and every
// matching rule gives one reason line.
func riskOf(st *State, c *Change) (string, []string) {
	var high, medium []string
	for _, g := range c.Grant.Integrations {
		if len(g.Write) > 0 {
			high = append(high, fmt.Sprintf("it can suggest changes in %s (%s), which you approve one by one", g.Name, hostOf(g.URL)))
		} else {
			medium = append(medium, fmt.Sprintf("it can read from %s (%s)", g.Name, hostOf(g.URL)))
		}
	}
	if c.reach != nil {
		if proposesSomething(c.reach.Proposes) {
			high = append(high, "its results can propose writes (send, pay, book, delete), each approved by you")
		}
		if proposesStanding(c.reach.Proposes) {
			high = append(high, "you may approve some of its writes ahead of time; those are sent without showing you their text")
		}
		for _, in := range c.reach.Integrations {
			medium = append(medium, "it can read from "+in)
		}
		if c.reach.Schedule != "" {
			medium = append(medium, "it runs by itself "+scheduleWords(c.reach.Schedule))
		}
	}
	if c.Slot != nil {
		medium = append(medium, "it stores a credential ("+c.Slot.Name+") that only Vornik can use")
	}
	// §18.7 row Medium: it sends task content to a remote model provider
	// (design §18.6 item 2).
	for _, m := range c.Grant.Models {
		medium = append(medium, fmt.Sprintf("it sends what the %s role works on to %s", m.Role, destinationWords(m.Destination)))
	}
	if c.Grant.MaxTotalUSD != nil {
		total := *c.Grant.MaxTotalUSD
		if total > highCeilingUSD || (st.CeilingUSD > 0 && total > 2*st.CeilingUSD) {
			high = append(high, fmt.Sprintf("it takes your assistant's monthly limit to $%s, from $%s", FormatUSD(total), FormatUSD(st.CeilingUSD)))
		}
	}
	if c.Grant.AddsUSD != nil && *c.Grant.AddsUSD > 0 {
		medium = append(medium, fmt.Sprintf("it lets your assistant spend $%s more a month", FormatUSD(*c.Grant.AddsUSD)))
	}
	switch {
	case len(high) > 0:
		return LevelHigh, append(high, medium...)
	case len(medium) > 0:
		return LevelMedium, medium
	}
	return LevelLow, []string{"it reaches nothing outside Vornik and spends within the limit you approved"}
}

// ExplainHostAction is the plain view of every host_action request (Hermes
// approval transport design §4.1). The level is always High: Hermes's own
// rules flagged the command as dangerous, and Vornik does not re-judge them
// from the phone; the command text, shown below it, tells a force-push from
// a recursive delete.
func ExplainHostAction() PlainView {
	return PlainView{
		Summary: "Hermes wants to run a command on its own computer that its safety rules flagged. Vornik cannot stop it; Hermes will run it only if you allow it.",
		Level:   LevelHigh,
		Reasons: []string{"Hermes's safety rules flagged this command as dangerous", "Hermes, not Vornik, runs it if you allow it"},
	}
}

func spendPhrase(c *Change) string {
	switch {
	case c.Grant.MaxTotalUSD != nil && c.Grant.AddsUSD != nil:
		return fmt.Sprintf("This lets your assistant spend up to $%s a month in all, $%s more than now.", FormatUSD(*c.Grant.MaxTotalUSD), FormatUSD(*c.Grant.AddsUSD))
	case c.Grant.AddsUSD != nil:
		return fmt.Sprintf("This lets your assistant spend $%s more a month, within the limit you approved.", FormatUSD(*c.Grant.AddsUSD))
	}
	return "Its spending stays within the limit you approved."
}

func firstIntegration(c *Change) IntegrationGrant {
	if len(c.Grant.Integrations) > 0 {
		return c.Grant.Integrations[0]
	}
	return IntegrationGrant{Name: "a service"}
}

func projectOfLocks(c *Change) string {
	for _, l := range c.Locks {
		if id, ok := strings.CutPrefix(l, "project:"); ok {
			return id
		}
	}
	return "a project"
}

// proposesStanding: a proposal declares standing (broker write-actions
// design, tier 2), so a covered write is sent unseen.
func proposesStanding(raw json.RawMessage) bool {
	var v []struct {
		Standing json.RawMessage `json:"standing"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	for _, p := range v {
		if len(p.Standing) > 0 && string(p.Standing) != "null" {
			return true
		}
	}
	return false
}

func proposesSomething(raw json.RawMessage) bool {
	var v []any
	return json.Unmarshal(raw, &v) == nil && len(v) > 0
}

// scheduleWords says a schedule signature ({cron, timezone, inputs}) in
// words, with the same describer the approval sentence uses.
func scheduleWords(sig string) string {
	var s struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal([]byte(sig), &s) != nil || s.Cron == "" {
		return "on a schedule"
	}
	w := describeCron(s.Cron)
	if s.Timezone != "" {
		w += " (" + s.Timezone + ")"
	}
	return w
}

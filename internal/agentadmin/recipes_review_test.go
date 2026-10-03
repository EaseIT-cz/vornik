package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// Review 20261003-e379 F1: a schedule an agent passes to install_recipe, and
// a cron variable, are bound exactly as define_workflow's schedule is (the
// registry's broker.schedule validation, run on the rendered workflow): at
// most hourly, an IANA zone, inputs valid against the recipe's input schema.
func TestInstallRecipe_AgentScheduleIsBoundLikeDefineWorkflow(t *testing.T) {
	cases := map[string]struct {
		in   InstallRecipeInput
		want string
	}{
		"sub-hourly schedule": {withSchedule(`{"cron":"*/5 * * * *","timezone":"UTC","inputs":{"since":"24h"}}`), "at most hourly"},
		"unknown timezone":    {withSchedule(`{"cron":"0 7 * * *","timezone":"Mars/Base","inputs":{"since":"24h"}}`), "timezone"},
		"inputs off schema":   {withSchedule(`{"cron":"0 7 * * *","timezone":"UTC","inputs":{"since":"forever"}}`), "schedule.inputs"},
		"sub-hourly variable": {withVar("digest_cron", "*/10 * * * *"), "at most hourly"},
		"unknown tz variable": {withVar("timezone", "Mars/Base"), "timezone"},
	}
	for name, tc := range cases {
		tr := shippedTree(t)
		c := tr.render(VerbInstallRecipe, tc.in)
		if c.Class != Refused || !strings.Contains(c.Reason, tc.want) {
			t.Errorf("%s: class %s, reason %q", name, c.Class, c.Reason)
		}
	}
}

func withSchedule(s string) InstallRecipeInput {
	in := shippedVars("inbox-digest")
	in.Schedule = json.RawMessage(s)
	return in
}

func withVar(name, value string) InstallRecipeInput {
	in := shippedVars("inbox-digest")
	raw, _ := json.Marshal(value)
	in.Variables[name] = raw
	return in
}

// Review 20261003-e379 F6: the pre-approval listing resolves the input the
// way the install does (strict decoding, the same variables), so it never
// lists a server for an input the install will refuse.
func TestRecipeListTargets_SharesTheInstallsResolution(t *testing.T) {
	r := rendererWith(t, "", map[string]string{"probe": probeRecipe})
	ok, _ := json.Marshal(probeInstall(nil))
	if got := r.RecipeListTargets(ok); len(got) != 1 || got[0] != probeURL {
		t.Fatalf("control: %v", got)
	}
	var m map[string]any
	_ = json.Unmarshal(ok, &m)
	m["extra"] = true
	unknown, _ := json.Marshal(m)
	if got := r.RecipeListTargets(unknown); got != nil {
		t.Fatalf("an input the install refuses (unknown field) was listed: %v", got)
	}
	m = map[string]any{"recipe": "probe", "project": "Not A Slug", "variables": map[string]string{"server_url": probeURL}}
	badProject, _ := json.Marshal(m)
	if got := r.RecipeListTargets(badProject); got != nil {
		t.Fatalf("an input the install refuses (bad project) was listed: %v", got)
	}
}

// Review 20261003-e379 F10: a hostile tool list for a recipe-installed
// server neither widens the approval nor smuggles a name: extra tools are
// dropped to the recipe's set, an unusable name refuses.
func TestApproveServerTools_RecipeServerIgnoresAHostileList(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail",
		Tools: []string{"gmail_get", "gmail_search", "gmail_send", "delete_everything", "files_export"}})
	tr.mustClass(c, Widening)
	if got := strings.Join(c.Grant.Integrations[0].Read, ","); got != "gmail_get,gmail_search" {
		t.Fatalf("approved %s", got)
	}
	c = tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail",
		Tools: []string{"gmail_get", "gmail_search", "x; rm -rf /"}})
	tr.mustClass(c, Refused)
}

package agentadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// Design §19 (recipes): the consolidated test list of §19.8 as amended by
// §19.9 to §19.11. Each test names its item.

// probeRecipe is a minimal recipe for the rule tests: one credential-less
// server (so the unauthenticated tool check applies), a string variable in
// the schedule's inputs, and link_hosts.
const probeRecipe = `name: probe
version: 1
envelope_version: 1
title: "Probe"
summary: "Probe summary."
returns: "what the probe found"
variables:
  server_url: {type: url, help: "the server"}
  topic: {type: string, default: "news", help: "a topic"}
needs:
  - {name: src, reads: "your source", url: "{{server_url}}", read_tools: [look, peek]}
link_hosts: [src.example]
team:
  - {name: reader, instructions: "Read the source.", tools: [file_write, mcp__src__look, mcp__src__peek]}
workflow:
  slug: probe
  purpose: "Probe the source"
  inputs: {type: object, additionalProperties: false, properties: {topic: {type: string, maxLength: 300, x-untrusted: true}}}
  steps:
    - {name: look, role: reader, instructions: "Look at the source."}
  egress:
    item_properties:
      note: {type: string, maxLength: 100}
schedule_default:
  cron: "0 7 * * *"
  timezone: "UTC"
  inputs: {topic: "{{topic}}"}
`

// recipeFS is the shipped templates and envelope plus the given recipes
// (name → recipe.yaml) and, when set, another envelope.json.
func recipeFS(t *testing.T, envelope string, recipes map[string]string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, f := range []string{tmplProject, tmplSwarm, tmplWorkflow, envelopeFile} {
		b, err := os.ReadFile("../../configs/agent-templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		fsys[f] = &fstest.MapFile{Data: b}
	}
	if envelope != "" {
		fsys[envelopeFile] = &fstest.MapFile{Data: []byte(envelope)}
	}
	for name, body := range recipes {
		fsys["recipes/"+name+"/recipe.yaml"] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func rendererWith(t *testing.T, envelope string, recipes map[string]string) *Renderer {
	t.Helper()
	r, err := NewRenderer(recipeFS(t, envelope, recipes))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func loadErr(t *testing.T, recipe string) string {
	t.Helper()
	_, err := NewRenderer(recipeFS(t, "", map[string]string{"probe": recipe}))
	if err == nil {
		return ""
	}
	return err.Error()
}

// probeTree is a tree on the probe catalogue with the project "personal"
// and the probe server listable without a credential.
func probeTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.r = rendererWith(t, "", map[string]string{"probe": probeRecipe})
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "personal", Purpose: "Personal automations"}))
	tr.advert[probeURL] = []string{"look", "other", "peek", "scan"}
	return tr
}

const probeURL = "https://src.example/mcp"

func probeInstall(vars map[string]string) InstallRecipeInput {
	in := InstallRecipeInput{Recipe: "probe", Project: "personal", Variables: map[string]json.RawMessage{}}
	for k, v := range vars {
		raw, _ := json.Marshal(v)
		in.Variables[k] = raw
	}
	if _, ok := vars["server_url"]; !ok {
		in.Variables["server_url"] = json.RawMessage(`"` + probeURL + `"`)
	}
	return in
}

func shippedTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "personal", Purpose: "Personal automations"}))
	return tr
}

func shippedVars(recipe string) InstallRecipeInput {
	vars := map[string]json.RawMessage{}
	if recipe != "agenda" {
		vars["mail_server_url"] = json.RawMessage(`"https://mail.example/mcp"`)
	}
	if recipe != "inbox-digest" {
		vars["calendar_server_url"] = json.RawMessage(`"https://cal.example/mcp"`)
	}
	return InstallRecipeInput{Recipe: recipe, Project: "personal", Variables: vars}
}

// Item 1: list_recipes lists the shipped recipes with their sentences,
// read_tools, link_hosts and envelope_version, and where each is installed.
func TestListRecipes_ShowsTheShippedCatalogue(t *testing.T) {
	tr := shippedTree(t)
	views := tr.r.ListRecipes(tr.state())
	if len(views) != 3 || views[0].Name != "agenda" || views[1].Name != "inbox-digest" || views[2].Name != "morning-brief" {
		t.Fatalf("recipes: %+v", views)
	}
	for _, v := range views {
		if v.EnvelopeVersion != 1 || len(v.LinkHosts) == 0 || v.Sentence == "" || len(v.Needs) == 0 || len(v.Returns.Fields) == 0 {
			t.Fatalf("%s: incomplete view %+v", v.Name, v)
		}
		for _, n := range v.Needs {
			if len(n.ReadTools) == 0 || !strings.Contains(n.WorksWith, n.ReadTools[0]) || !strings.Contains(v.Sentence, "<"+strings.Trim(n.URL, "{}")+">") {
				t.Fatalf("%s: need %+v, sentence %q", v.Name, n, v.Sentence)
			}
		}
		if len(v.Installed) != 0 {
			t.Fatalf("%s is listed as installed before any install", v.Name)
		}
	}
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))
	views = tr.r.ListRecipes(tr.state())
	if got := views[1].Installed; len(got) != 1 || got[0].Project != "hermes--personal" || got[0].Version != 1 || got[0].Workflow != "hermes--personal--inbox-digest" {
		t.Fatalf("installed: %+v", got)
	}
}

// Item 2: a fresh install, every entry new, is one change with the
// recipe's declared reach, filing one approval naming every destination,
// then one credential request per credential.
func TestInstallRecipe_FreshInstallIsOneWideningChange(t *testing.T) {
	tr := shippedTree(t)
	c := tr.render(VerbInstallRecipe, shippedVars("morning-brief"))
	tr.mustClass(c, Widening)
	if len(c.Ops) != 3 {
		t.Fatalf("ops: %v", c.Ops)
	}
	for _, url := range []string{"https://mail.example/mcp", "https://cal.example/mcp"} {
		if !strings.Contains(c.Sentence, url) {
			t.Fatalf("the sentence does not name %s: %s", url, c.Sentence)
		}
	}
	if len(c.Grant.Integrations) != 2 || !c.Grant.Integrations[0].ReadPending || !c.Grant.Integrations[1].ReadPending {
		t.Fatalf("grants: %+v", c.Grant.Integrations)
	}
	if h := c.Grant.Workflows["hermes--personal--morning-brief"]; h == "" || h != c.reach.Hash() {
		t.Fatalf("the workflow grant is not its reach: %v", c.Grant.Workflows)
	}
	if got := c.Credentials; len(got) != 2 || got[0].Name != "CALENDAR_TOKEN" || got[1].Name != "MAIL_TOKEN" || got[0].Kind != CredentialSecret {
		t.Fatalf("credential follow-ups: %+v", got)
	}
	if c.Plain == nil || !strings.Contains(c.Plain.Summary, "Morning brief") {
		t.Fatalf("plain view: %+v", c.Plain)
	}
}

// Item 3: substitution is by value, only into needs and schedule_default,
// and every variable is used.
func TestRecipeLoad_VariablesOnlyWhereTheyConnect(t *testing.T) {
	cases := map[string]string{
		"in an instruction": strings.Replace(probeRecipe, `instructions: "Look at the source."`, `instructions: "Look at {{topic}}."`, 1),
		"in a tool":         strings.Replace(probeRecipe, `mcp__src__peek]}`, `mcp__src__peek, "{{topic}}"]}`, 1),
		"in a schema":       strings.Replace(probeRecipe, `note: {type: string, maxLength: 100}`, `note: {type: string, maxLength: 100, description: "{{topic}}"}`, 1),
		"spliced into text": strings.Replace(probeRecipe, `url: "{{server_url}}"`, `url: "https://{{topic}}/mcp"`, 1),
		"undeclared":        strings.Replace(probeRecipe, `inputs: {topic: "{{topic}}"}`, `inputs: {topic: "{{nope}}"}`, 1),
		"unreferenced":      strings.Replace(probeRecipe, `inputs: {topic: "{{topic}}"}`, `inputs: {topic: "fixed"}`, 1),
	}
	for name, body := range cases {
		if body == probeRecipe {
			t.Fatalf("%s: the case did not change the recipe", name)
		}
		if err := loadErr(t, body); err == "" {
			t.Errorf("%s: the catalogue loaded", name)
		}
	}
	if err := loadErr(t, probeRecipe); err != "" {
		t.Fatalf("the control recipe does not load: %s", err)
	}
}

// Item 3 (by value) and item 7 (sentence honesty): a hostile string stays
// one scalar in its one field, and the approval sentence is the same apart
// from the daemon-filled slots.
func TestInstallRecipe_HostileStringIsOneScalarAndChangesNoSentence(t *testing.T) {
	hostile := `a": "b" # {c} {{server_url}}`
	render := func(topic string) (Change, *registry.Workflow) {
		tr := probeTree(t)
		c := tr.render(VerbInstallRecipe, probeInstall(map[string]string{"topic": topic}))
		tr.mustClass(c, Widening)
		for _, op := range c.Ops {
			if strings.HasPrefix(op.Path, "workflows/") {
				wf, err := registry.ParseWorkflowMarkdown([]byte(op.Content), op.Path)
				if err != nil {
					t.Fatal(err)
				}
				return c, wf
			}
		}
		t.Fatal("no workflow op")
		return c, nil
	}
	benign, _ := render("news")
	evil, wf := render(hostile)
	if got := wf.Broker.Schedule.Inputs["topic"]; got != hostile || len(wf.Broker.Schedule.Inputs) != 1 {
		t.Fatalf("the hostile value is not one scalar in its field: %#v", wf.Broker.Schedule.Inputs)
	}
	// The slots: the schedule clause carries the inputs as canonical JSON
	// (a §7.6 reach slot); everything else must be byte-identical.
	slot := regexp.MustCompile(`with \{.*\}\.`)
	if a, b := slot.ReplaceAllString(benign.Sentence, "with <inputs>."), slot.ReplaceAllString(evil.Sentence, "with <inputs>."); a != b {
		t.Fatalf("a variable changed the sentence outside its slot:\n%s\n%s", a, b)
	}
	inputs, _ := json.Marshal(map[string]string{"topic": hostile})
	if !strings.Contains(evil.Sentence, string(inputs)) {
		t.Fatalf("the hostile value is not JSON-quoted in its slot: %s", evil.Sentence)
	}
}

// Item 4: reuse by URL and by tools; a narrowing applies without approval.
func TestInstallRecipe_ReuseUpgradeAndNarrowing(t *testing.T) {
	v2 := func(read, tools string) string {
		s := strings.Replace(probeRecipe, "version: 1\nenvelope", "version: 2\nenvelope", 1)
		s = strings.Replace(s, "read_tools: [look, peek]", "read_tools: ["+read+"]", 1)
		return strings.Replace(s, "tools: [file_write, mcp__src__look, mcp__src__peek]", "tools: [file_write, "+tools+"]", 1)
	}
	install := func(tr *tree) Change { return tr.render(VerbInstallRecipe, probeInstall(nil)) }

	t.Run("same URL and tools is inert and files nothing", func(t *testing.T) {
		tr := probeTree(t)
		tr.apply(install(tr))
		c := install(tr)
		tr.mustClass(c, Inert)
		if len(c.Ops) != 0 || len(c.Grant.Integrations) != 0 || len(c.Narrow.RemovedTools) != 0 {
			t.Fatalf("a reinstall is not inert: %+v", c)
		}
	})
	t.Run("an added tool is widening and names it", func(t *testing.T) {
		tr := probeTree(t)
		tr.apply(install(tr))
		tr.r = rendererWith(t, "", map[string]string{"probe": v2("look, peek, scan", "mcp__src__look, mcp__src__peek, mcp__src__scan")})
		c := install(tr)
		tr.mustClass(c, Widening)
		if !strings.Contains(c.Sentence, "now also with scan") || !strings.Contains(c.Sentence, "from version 1 to version 2") {
			t.Fatalf("sentence: %s", c.Sentence)
		}
		if len(c.Grant.Integrations) != 1 || strings.Join(c.Grant.Integrations[0].Read, ",") != "look,peek,scan" {
			t.Fatalf("the tools approval is not in the change: %+v", c.Grant.Integrations)
		}
	})
	t.Run("tools only removed is an inert narrowing", func(t *testing.T) {
		tr := probeTree(t)
		tr.apply(install(tr))
		tr.r = rendererWith(t, "", map[string]string{"probe": v2("look", "mcp__src__look")})
		c := install(tr)
		tr.mustClass(c, Inert)
		if len(c.Narrow.RemovedTools) != 1 || strings.Join(c.Narrow.RemovedTools[0].Tools, ",") != "peek" || !strings.Contains(c.Sentence, "no longer uses peek") {
			t.Fatalf("narrowing: %+v %q", c.Narrow, c.Sentence)
		}
		tr.apply(c)
		if got := tr.approvals["hermes--personal"]["src"].Read; strings.Join(got, ",") != "look" {
			t.Fatalf("the removed tool is still approved: %v", got)
		}
	})
	t.Run("added and removed is widening and the removal rides with it", func(t *testing.T) {
		tr := probeTree(t)
		tr.apply(install(tr))
		tr.r = rendererWith(t, "", map[string]string{"probe": v2("look, scan", "mcp__src__look, mcp__src__scan")})
		c := install(tr)
		tr.mustClass(c, Widening)
		if !strings.Contains(c.Sentence, "now also with scan and no longer with peek") || len(c.Narrow.RemovedTools) != 1 {
			t.Fatalf("mixed: %q %+v", c.Sentence, c.Narrow)
		}
	})
	t.Run("another URL is refused naming the server; after remove it is a fresh install", func(t *testing.T) {
		tr := probeTree(t)
		tr.apply(install(tr))
		const other = "https://other.example/mcp"
		tr.advert[other] = []string{"look", "peek"}
		c := tr.render(VerbInstallRecipe, probeInstall(map[string]string{"server_url": other}))
		tr.mustClass(c, Refused)
		if !strings.Contains(c.Reason, `named "src" at `+probeURL) {
			t.Fatalf("reason: %s", c.Reason)
		}
		tr.apply(tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "personal", ID: "src"}))
		c = tr.render(VerbInstallRecipe, probeInstall(map[string]string{"server_url": other}))
		tr.mustClass(c, Widening)
		if !strings.Contains(c.Sentence, other) {
			t.Fatalf("the fresh install does not name the new URL: %s", c.Sentence)
		}
	})
}

// Item 5: roles install as <recipe>-<role>; a role of that full name not
// installed by the recipe refuses the whole install.
func TestInstallRecipe_RolesArePrefixedAndACollisionRefusesAtomically(t *testing.T) {
	tr := probeTree(t)
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.apply(c)
	st := tr.state()
	if roles := strings.Join(roleNames(st.Projects["hermes--personal"].Swarm.Roles), ","); roles != "worker,probe-reader" {
		t.Fatalf("roles: %s", roles)
	}

	tr = probeTree(t)
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "personal", Roles: []RoleInput{{Name: "probe-reader", Instructions: "Mine.", Tools: []string{"file_read"}}}}))
	before := len(tr.files)
	c = tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Refused)
	if !strings.Contains(c.Reason, `role named "probe-reader"`) || len(c.Ops) != 0 || len(tr.files) != before {
		t.Fatalf("collision: %q ops %d", c.Reason, len(c.Ops))
	}
}

func roleNames(rs []RoleSpec) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// Item 8: link points only at the recipe's hosts and at a resource; a
// recipe without link_hosts has no link; a token-shaped id is withheld by
// the per-leaf secret scan.
func TestInstallRecipe_LinkHostsAndIDs(t *testing.T) {
	tr := shippedTree(t)
	c := tr.render(VerbInstallRecipe, shippedVars("inbox-digest"))
	var wf *registry.Workflow
	for _, op := range c.Ops {
		if strings.HasPrefix(op.Path, "workflows/") {
			wf, _ = registry.ParseWorkflowMarkdown([]byte(op.Content), op.Path)
		}
	}
	doc := func(link, id string) []byte {
		item := map[string]any{"source": "mail", "id": id, "received_at": "2026-10-03T07:00:00Z", "from_domain": "acme.example",
			"category": "invoice", "needs_reply": true, "one_line": "An invoice is due."}
		if link != "" {
			item["link"] = link
		}
		b, _ := json.Marshal(map[string]any{"status": "ok", "as_of": "2026-10-03T07:01:00Z", "items": []any{item},
			"counts": map[string]any{"total": 1, "shown": 1, "suppressed": 0}})
		return b
	}
	if v := registry.ValidateBrokerEgress(wf, doc("https://mail.google.com/mail/u/0/#inbox/abc", "m1")); v.Class != "" {
		t.Fatalf("a valid answer was refused: %v", v.Messages)
	}
	for _, bad := range []string{"https://evil.example/mail.google.com/x", "https://mail.google.com.evil.example/x", "https://mail.google.com", "http://mail.google.com/x"} {
		if v := registry.ValidateBrokerEgress(wf, doc(bad, "m1")); v.Class != registry.EgressClassSchema {
			t.Errorf("link %q was accepted", bad)
		}
	}
	noLinks := rendererWith(t, "", map[string]string{"probe": strings.Replace(probeRecipe, "link_hosts: [src.example]\n", "", 1)})
	for _, rec := range noLinks.Catalogue().Recipes {
		if strings.Contains(rec.EgressJSON(), `"link"`) {
			t.Fatalf("a recipe without link_hosts has a link: %s", rec.EgressJSON())
		}
	}
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := egressscan.ScanJSON(d, doc("", "AKIAQWERTYUIOPASDFGH"))
	if f, blocked := egressscan.Blocking(fs); err != nil || !blocked || f.Path != "$.items[0].id" {
		t.Fatalf("a token-shaped id was not withheld: %+v %v", fs, err)
	}
}

// Item 9: the unauthenticated tool check refuses before approval; for a
// credential-first server the tools-approval check files nothing when the
// server lacks a recipe tool, and approves exactly the recipe's tools.
func TestInstallRecipe_FamilyChecks(t *testing.T) {
	tr := probeTree(t)
	tr.advert[probeURL] = []string{"look", "other"}
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Refused)
	if !strings.Contains(c.Reason, "does not offer peek") {
		t.Fatalf("reason: %s", c.Reason)
	}

	tr = shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))
	c = tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail", Tools: []string{"gmail_search", "gmail_send"}})
	tr.mustClass(c, Refused)
	if !strings.Contains(c.Reason, "does not offer gmail_get") {
		t.Fatalf("reason: %s", c.Reason)
	}
	c = tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail", Tools: []string{"gmail_get", "gmail_search", "gmail_send"}})
	tr.mustClass(c, Widening)
	if got := c.Grant.Integrations[0].Read; strings.Join(got, ",") != "gmail_get,gmail_search" || strings.Contains(c.Sentence, "gmail_send") {
		t.Fatalf("the tools approval is not the recipe's tools: %v %q", got, c.Sentence)
	}
}

// Item 10: the envelope's names are reserved; an envelope_version bump is a
// widening upgrade (the egress changed); the same version is inert (item 4).
func TestRecipeEnvelope_ReservedNamesAndVersion(t *testing.T) {
	for name, body := range map[string]string{
		"top-level status": strings.Replace(probeRecipe, "    item_properties:", "    properties:\n      status: {type: boolean}\n    item_properties:", 1),
		"top-level errors": strings.Replace(probeRecipe, "    item_properties:", "    properties:\n      errors: {type: boolean}\n    item_properties:", 1),
		"item link":        strings.Replace(probeRecipe, "      note: {type: string, maxLength: 100}", "      link: {type: string, maxLength: 100}", 1),
		"item source":      strings.Replace(probeRecipe, "      note: {type: string, maxLength: 100}", "      source: {type: string, maxLength: 100}", 1),
	} {
		if err := loadErr(t, body); !strings.Contains(err, "envelope") {
			t.Errorf("%s: %q", name, err)
		}
	}
	tr := probeTree(t)
	tr.apply(tr.render(VerbInstallRecipe, probeInstall(nil)))
	env, _ := os.ReadFile("../../configs/agent-templates/recipes/envelope.json")
	env2 := strings.Replace(strings.Replace(string(env), `"version": 1`, `"version": 2`, 1), `"maxItems": 50`, `"maxItems": 40`, 1)
	bumped := strings.Replace(strings.Replace(probeRecipe, "envelope_version: 1", "envelope_version: 2", 1), "version: 1\n", "version: 2\n", 1)
	tr.r = rendererWith(t, env2, map[string]string{"probe": bumped})
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Widening)
	if _, ok := c.Grant.Workflows["hermes--personal--probe"]; !ok {
		t.Fatalf("the envelope change does not re-bind the workflow: %+v", c.Grant)
	}
	if err := loadErr(t, strings.Replace(probeRecipe, "envelope_version: 1", "envelope_version: 2", 1)); !strings.Contains(err, "envelope_version") {
		t.Fatalf("a recipe pinned to another envelope loaded: %q", err)
	}
}

// §19.8 F2 and §19.10/§19.11: the catalogue refuses a recipe missing a
// required field, a role tool outside its entry's read_tools, a read tool
// no role holds, and one credential named with two kinds.
func TestRecipeLoad_SchemaRules(t *testing.T) {
	two := strings.Replace(probeRecipe, "  - {name: src, reads: \"your source\", url: \"{{server_url}}\", read_tools: [look, peek]}",
		"  - {name: src, reads: \"your source\", url: \"{{server_url}}\", read_tools: [look, peek], credential: {name: OAUTH_OTHER, kind: secret}}\n"+
			"  - {name: other, reads: \"more\", url: \"https://o.example/mcp\", read_tools: [look], credential: {name: OAUTH_OTHER, kind: oauth}}", 1)
	two = strings.Replace(two, "mcp__src__peek]", "mcp__src__peek, mcp__other__look]", 1)
	for name, body := range map[string]string{
		"no envelope_version":       strings.Replace(probeRecipe, "envelope_version: 1\n", "", 1),
		"no read_tools":             strings.Replace(probeRecipe, ", read_tools: [look, peek]", "", 1),
		"a tool outside read_tools": strings.Replace(probeRecipe, "mcp__src__peek]", "mcp__src__peek, mcp__src__scan]", 1),
		"a tool of no entry":        strings.Replace(probeRecipe, "mcp__src__peek]", "mcp__src__peek, mcp__nope__look]", 1),
		"a read tool nobody holds":  strings.Replace(probeRecipe, "tools: [file_write, mcp__src__look, mcp__src__peek]", "tools: [file_write, mcp__src__look]", 1),
		"one credential two kinds":  two,
		"an unknown field":          probeRecipe + "extra: 1\n",
	} {
		if err := loadErr(t, body); err == "" {
			t.Errorf("%s: the catalogue loaded", name)
		}
	}
}

// Item 11: each shipped recipe, rendered with fixed variables and loaded
// through the registry, runs as a broker workflow; its reach and its egress
// are pinned, so a catalogue change is a visible, reviewed change here.
func TestShippedRecipes_RenderLoadAndPin(t *testing.T) {
	pins := map[string]struct {
		integrations, credentials string
		schedule                  bool
	}{
		"inbox-digest":  {"mail", "mail=secret://hermes/MAIL_TOKEN", true},
		"agenda":        {"calendar", "calendar=secret://hermes/CALENDAR_TOKEN", false},
		"morning-brief": {"calendar,mail", "calendar=secret://hermes/CALENDAR_TOKEN,mail=secret://hermes/MAIL_TOKEN", true},
	}
	egressPins := map[string]string{
		"agenda":        "4ebd3e67a709c32cbce1d0f74746b4c979bcc3296c54ed6a874458e883ca32f8",
		"inbox-digest":  "e2bc4dd9abd08dc9444f8496077280ce9b94768164184cd6fa477875ed8392c8",
		"morning-brief": "a86f3061da7c99bdb6dd52968ff0a8dbb806c2bf07dbfd66740d0efab8919f4a",
	}
	for name, pin := range pins {
		t.Run(name, func(t *testing.T) {
			tr := shippedTree(t)
			c := tr.render(VerbInstallRecipe, shippedVars(name))
			tr.mustClass(c, Widening)
			tr.apply(c)
			st := tr.state()
			p := st.Projects["hermes--personal"]
			wf := st.Workflows["hermes--personal--"+name].Loaded
			if err := registry.CheckBrokerRunnable(p.Loaded, wf, p.LoadedSwarm); err != nil {
				t.Fatalf("not runnable as a broker workflow: %v", err)
			}
			sig, err := SignatureOf(p.Loaded, p.LoadedSwarm, wf)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(sig.Integrations, ","); got != pin.integrations {
				t.Errorf("integrations %s, pinned %s", got, pin.integrations)
			}
			if got := strings.Join(sig.Credentials, ","); got != pin.credentials {
				t.Errorf("credentials %s, pinned %s", got, pin.credentials)
			}
			if (sig.Schedule != "") != pin.schedule {
				t.Errorf("schedule %q", sig.Schedule)
			}
			if sig.Hash() != c.Grant.Workflows[wf.ID] {
				t.Error("the loaded workflow's reach is not the approved one")
			}
			sum := sha256.Sum256([]byte(tr.r.Catalogue().Get(name).EgressJSON()))
			if got := hex.EncodeToString(sum[:]); got != egressPins[name] {
				t.Errorf("egress changed: sha256 %s, pinned %s; review the change and update the pin", got, egressPins[name])
			}
			if n, v := InstalledRecipe(wf); n != name || v != 1 {
				t.Errorf("installed marker %s %d", n, v)
			}
		})
	}
}

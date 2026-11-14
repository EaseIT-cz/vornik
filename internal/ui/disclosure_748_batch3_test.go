package ui

// #748 rollout batch 3 (web-ui-design.md "Progressive disclosure across the
// console", I). Found at: production; the swarm settings form showed 65,067
// characters (13 role cards, 4 sections open each, every field's help a
// paragraph repeated per role) and admin skills 43,832 (73 cards with their
// metadata, usage and ratings lines open). These render the real templates.

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/ui/assetschema"
)

func renderTpl(t *testing.T, name string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := NewServer().templates.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

func swarmFormFixture(errOn int) SwarmSchemaConfigData {
	coll, _ := assetschema.SwarmSchema().CollectionByPath("roles")
	d := SwarmSchemaConfigData{Title: "Swarm", SwarmID: "s1", RolesTitle: "Roles"}
	for i, name := range []string{"lead", "writer", "reviewer"} {
		prefix := "roles[" + string(rune('0'+i)) + "]"
		values := map[string]string{prefix + ".name": name, prefix + ".model": "model-" + name}
		errs := map[string]string{}
		if i == errOn {
			errs[prefix+".model"] = "model is not on the catalogue"
		}
		d.RoleCards = append(d.RoleCards, swarmRoleCard{Index: i, Name: name, Sections: buildSchemaSections(coll.ItemSchema, prefix, values, errs)})
	}
	return d
}

func TestBatch3_748_SwarmRoleCardsFoldTheirSettings(t *testing.T) {
	body := renderTpl(t, "swarm_schema_config.html", swarmFormFixture(2))
	hidden := closedDetailsBodies(body)
	for _, name := range []string{"lead", "writer"} {
		ctl := `name="roles[` + map[string]string{"lead": "0", "writer": "1"}[name] + `].model"`
		if !strings.Contains(body, ctl) {
			t.Fatalf("%s's model control missing", name)
		}
		if !strings.Contains(hidden, ctl) {
			t.Errorf("%s's settings must be inside a closed disclosure", name)
		}
	}
	if strings.Contains(hidden, `name="roles[2].model"`) {
		t.Error("a card holding a field error must render open")
	}
	for _, m := range []string{"model-lead", "model-writer"} {
		if !regexp.MustCompile(`role-card-model[^>]*>` + m + `<`).MatchString(body) {
			t.Errorf("the card header must show its model %q", m)
		}
	}
	if strings.Contains(hidden, `data-on-click="swarmMoveCard"`) {
		t.Error("the move and remove controls stay outside the disclosure")
	}
	// Field help is a hint, not a paragraph under the control.
	if regexp.MustCompile(`<p class="text-\[11px\] text-gray-500">Number of agents for this role\.</p>`).MatchString(body) {
		t.Error("field help must not be a visible paragraph")
	}
	if !strings.Contains(body, `vk-hint-pop" role="tooltip">Number of agents for this role.`) {
		t.Error("field help must be in a hint")
	}
	if !strings.Contains(body, "model is not on the catalogue") {
		t.Error("a field error stays visible")
	}
	// The form posts the same names (review-20261113-3d41 M2): every card
	// keeps roles[i].<path> and the same set of paths, also while closed.
	nameRe := regexp.MustCompile(`name="roles\[(\d)\]\.([A-Za-z0-9_.]+)"`)
	paths := map[string]map[string]bool{}
	for _, m := range nameRe.FindAllStringSubmatch(body, -1) {
		if paths[m[1]] == nil {
			paths[m[1]] = map[string]bool{}
		}
		paths[m[1]][m[2]] = true
	}
	if len(paths) != 3 {
		t.Fatalf("cards with named inputs: got %d, want 3", len(paths))
	}
	for i := range paths {
		if len(paths[i]) != len(paths["0"]) || !paths[i]["name"] || !paths[i]["model"] {
			t.Errorf("card %s posts %d paths, card 0 posts %d; each needs name and model", i, len(paths[i]), len(paths["0"]))
		}
	}
}

// The shared schemaRow define, by source: help is a hint, never a paragraph
// under the control. Its users are the swarm role card, the swarm form, the
// workflow step card and the workflow schema form (review-20261113-3d41 M3).
func TestBatch3_748_SchemaRowHelpIsAHint(t *testing.T) {
	raw, err := os.ReadFile("templates/swarm_role_card.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	i := strings.Index(src, `{{define "schemaRow"}}`)
	if i < 0 {
		t.Fatal("schemaRow define not found")
	}
	end := strings.Index(src[i:], "{{end}}\n\n")
	if end < 0 {
		t.Fatal("schemaRow define's end not found")
	}
	def := src[i : i+end]
	// #753 part B moved the old text-[11px] onto a named step, so this
	// guard matches any class that renders the help as a paragraph.
	if regexp.MustCompile(`<p class="[^"]*">\{\{\.Field\.Help\}\}</p>`).MatchString(def) {
		t.Error("schemaRow renders help as a paragraph")
	}
	if !strings.Contains(def, `{{template "hintOpen"`) || !strings.Contains(def, `{{.Field.Help}}{{template "hintClose"}}`) {
		t.Error("schemaRow renders help in a hint")
	}
	// Beside the label, not inside it (review-20261113-5a93 m1): the hint
	// opens after the label closes.
	if h, l := strings.Index(def, `{{template "hintOpen"`), strings.Index(def, `</label>`); l < 0 || h < l {
		t.Errorf("the hint must follow the closing </label> (hint at %d, </label> at %d)", h, l)
	}
}

func TestBatch3_748_InboxAndProposalStayClosed(t *testing.T) {
	for _, f := range []string{"inbox.html", "admin_workflow_proposal.html"} {
		raw, err := os.ReadFile("templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")
		for _, tag := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(markup, -1) {
			if strings.Contains(tag, " open") {
				t.Errorf("%s: %s renders open; it was closed by default", f, tag)
			}
		}
	}
}

func TestBatch3_748_ANewRoleCardOpens(t *testing.T) {
	coll, _ := assetschema.SwarmSchema().CollectionByPath("roles")
	card := swarmRoleCard{Index: 3, Sections: buildSchemaSections(coll.ItemSchema, "roles[3]", defaultsAsValues(coll.ItemSchema, "roles[3]"), nil)}
	body := renderTpl(t, "swarm_role_card.html", card)
	if strings.Contains(closedDetailsBodies(body), `name="roles[3].name"`) {
		t.Error("a new card (no name yet) renders its settings open")
	}
}

func TestBatch3_748_SkillCardsFoldTheirMetadata(t *testing.T) {
	d := AdminSkillsData{Available: true, Rows: []AdminSkillRow{
		{ID: "k1", Name: "low", Version: 2, Maturity: "draft", Description: "Low lift skill", ProjectID: "p1", UsageFired: 9, CanApprove: true, CanReject: true, Rating: &AdminSkillRating{Verdict: "low_lift", Detail: []string{"p1/wf: 2 of 9"}}},
		{ID: "k2", Name: "unrated", Version: 1, Maturity: "active", Description: "Unrated skill", ProjectID: "p1", Rating: &AdminSkillRating{Verdict: "not_measurable"}},
		{ID: "k3", Name: "plain", Version: 1, Maturity: "trusted", Description: "Plain skill", ProjectID: "p2"},
	}}
	body := renderTpl(t, "admin_skills.html", d)
	hidden := closedDetailsBodies(body)
	for _, s := range []string{"usage: fired", "· roles"} {
		if strings.Count(body, s) != 3 || strings.Count(hidden, s) != 3 {
			t.Errorf("%q: every card's line must sit inside a closed disclosure (body %d, hidden %d)", s, strings.Count(body, s), strings.Count(hidden, s))
		}
	}
	for _, action := range []string{"✅ Approve", "Make global"} {
		if !strings.Contains(body, action) || strings.Contains(hidden, action) {
			t.Errorf("the %q action stays visible", action)
		}
	}
	if !regexp.MustCompile(`vk-disc-meta">[^<]*low_lift`).MatchString(body) {
		t.Error("a low_lift verdict stays in sight, in its card's summary")
	}
	if regexp.MustCompile(`vk-disc-meta">[^<]*not_measurable`).MatchString(body) {
		t.Error("an unrated card's summary carries no verdict")
	}
	d.Rows[2].IsGlobal = true
	body = renderTpl(t, "admin_skills.html", d)
	hidden = closedDetailsBodies(body)
	if !strings.Contains(body, "Affects ALL projects") || strings.Contains(hidden, "Affects ALL projects") {
		t.Error("the \"affects ALL projects\" warning stays visible")
	}
	for _, desc := range []string{"Low lift skill", "Unrated skill", "Plain skill"} {
		if strings.Contains(hidden, desc) {
			t.Errorf("the description %q stays visible", desc)
		}
	}
}

func TestBatch3_748_EveryDisclosureIsShared(t *testing.T) {
	classOf := regexp.MustCompile(`class="([^"]*)"`)
	// swarm_edit.html has no disclosure (0 examined): it is in the list so
	// one added later must use the shared classes.
	for _, f := range []string{"swarm_schema_config.html", "swarm_role_card.html", "admin_skills.html", "swarm_edit.html", "inbox.html", "admin_workflow_proposal.html"} {
		raw, err := os.ReadFile("templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")
		tags := regexp.MustCompile(`<details\b[^>]*>`).FindAllString(markup, -1)
		for _, tag := range tags {
			if m := classOf.FindStringSubmatch(tag); m == nil || (m[1] != "vk-disc" && m[1] != "vk-disc vk-disc-row") {
				t.Errorf("%s: %s: a disclosure carries exactly the component's classes", f, tag)
			}
		}
		for _, sum := range regexp.MustCompile(`<summary\b[^>]*>`).FindAllString(markup, -1) {
			if m := classOf.FindStringSubmatch(sum); m == nil || strings.Fields(m[1])[0] != "vk-disc-sum" {
				t.Errorf("%s: %s: a summary starts with vk-disc-sum", f, sum)
			}
		}
		if strings.Contains(markup, "group-open:") {
			t.Errorf("%s still uses group-open:", f)
		}
		t.Logf("%s: %d disclosures examined", f, len(tags))
	}
}

// #748 K: execution's disclosures are shared and no template carries
// panel-ref any more; its CSS rule is retired.
func TestBatch4_748_ExecutionSharedAndPanelRefRetired(t *testing.T) {
	raw, err := os.ReadFile("templates/execution.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")
	classOf := regexp.MustCompile(`class="([^"]*)"`)
	tags := regexp.MustCompile(`<details\b[^>]*>`).FindAllString(markup, -1)
	if len(tags) != 5 {
		t.Errorf("execution disclosures examined: %d, want 5", len(tags))
	}
	for _, tag := range tags {
		if m := classOf.FindStringSubmatch(tag); m == nil || (m[1] != "vk-disc" && m[1] != "vk-disc vk-disc-row") {
			t.Errorf("%s: a disclosure carries exactly the component's classes", tag)
		}
	}
	for _, sum := range regexp.MustCompile(`<summary\b[^>]*>`).FindAllString(markup, -1) {
		if m := classOf.FindStringSubmatch(sum); m == nil || len(strings.Fields(m[1])) == 0 || strings.Fields(m[1])[0] != "vk-disc-sum" {
			t.Errorf("%s: a summary starts with vk-disc-sum", sum)
		}
	}
	// No template, Go file or shipped script carries panel-ref
	// (review-20261113-8a15): directories are skipped, a read error fails.
	examined := 0
	for _, dir := range []string{"templates", ".", "static"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			n := e.Name()
			source := strings.HasSuffix(n, ".html") || strings.HasSuffix(n, ".go") || strings.HasSuffix(n, ".js")
			if e.IsDir() || strings.HasSuffix(n, "_test.go") || !source {
				continue
			}
			b, err := os.ReadFile(dir + "/" + n)
			if err != nil {
				t.Fatal(err)
			}
			examined++
			code := regexp.MustCompile(`(?s)<!--.*?-->|\{\{/\*.*?\*/\}\}|//[^\n]*`).ReplaceAllString(string(b), "")
			if regexp.MustCompile(`class="[^"]*\bpanel-ref\b`).MatchString(code) || strings.Contains(code, ".panel-ref") || strings.Contains(code, `"panel-ref`) {
				t.Errorf("%s/%s still carries panel-ref", dir, n)
			}
		}
	}
	if examined < 100 {
		t.Fatalf("examined %d files for panel-ref; the scan looks empty", examined)
	}
}

// The page's top-level sections open when basic or holding an error
// (#748 I; review-20261113-5a93 m3).
func TestBatch3_748_TopSectionsOpenOnError(t *testing.T) {
	adv := schemaSectionView{Title: "Advanced bits", Advanced: true, Rows: []schemaFieldRow{{Name: "x", Err: "bad value"}}}
	clean := schemaSectionView{Title: "Quiet bits", Advanced: true}
	basic := schemaSectionView{Title: "Basics"}
	body := renderTpl(t, "swarm_schema_config.html", SwarmSchemaConfigData{Title: "Swarm", SwarmID: "s1", TopSections: []schemaSectionView{basic, adv, clean}})
	for _, c := range []struct {
		title string
		open  bool
	}{{"Basics", true}, {"Advanced bits", true}, {"Quiet bits", false}} {
		i := strings.Index(body, ">"+c.title+"</h2>")
		if i < 0 {
			t.Fatalf("section %q not rendered", c.title)
		}
		tag := body[strings.LastIndex(body[:i], "<details"):i]
		gt := strings.Index(tag, ">")
		if gt < 0 {
			t.Fatalf("section %q: unterminated <details> tag", c.title)
		}
		if got := strings.Contains(tag[:gt], " open"); got != c.open {
			t.Errorf("section %q: open=%v, want %v", c.title, got, c.open)
		}
	}
}

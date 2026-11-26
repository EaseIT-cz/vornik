package ui

// GitHub #854 (UI design §24): every `.ID` printed as text in a console
// template is either shortened (shortID) or on the list below with its
// reason. A new page that prints an ID whole has to add itself here, which
// makes it a decision instead of an accident.
//
// Found at: production (operator, 2026-11-19, #848: long generated IDs take
// too much space), observed at unit (the templates' text).

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The reasons of UI design §24.
const (
	chosenByAPerson = "1 chosen by a person"
	ownRecord       = "2 the page's own record"
	quotedOutside   = "3 quoted outside the console"
	dataNotText     = "4 data, not text"
)

// wholeIDs854 is template → expression → reason, for the places that keep
// an ID whole. The expression is the action as written, so editing other
// lines of a template does not break the test. The count of each is pinned
// too: a second copy on a new line is a new decision.
var wholeIDs854 = map[string]map[string]struct {
	reason string
	count  int
}{
	"admin_agents.html":            {"{{.ID}}": {chosenByAPerson, 1}},
	"admin_users.html":             {"{{.ID}}": {chosenByAPerson, 1}},
	"operator_accounts.html":       {"{{.ID}}": {chosenByAPerson, 1}},
	"memory_index.html":            {"{{.ID}}": {chosenByAPerson, 1}},
	"setup.html":                   {"{{.ID}}": {chosenByAPerson, 1}},
	"swarms.html":                  {"{{.ID}}": {chosenByAPerson, 2}},
	"workflows.html":               {"{{.ID}}": {chosenByAPerson, 2}},
	"workflow_edit.html":           {"{{.ID}}": {chosenByAPerson, 2}},
	"workflow_graph.html":          {"{{.ID}}": {chosenByAPerson, 1}},
	"composer_preview.html":        {"{{.ID}}": {chosenByAPerson, 1}},
	"project_detail.html":          {"{{.Project.ID}}": {chosenByAPerson, 5}},
	"projects.html":                {"{{.Project.ID}}": {chosenByAPerson, 2}},
	"execution.html":               {"{{.Execution.ID}}": {ownRecord, 1}},
	"project_document_detail.html": {"{{.Document.ID}}": {ownRecord, 1}},
	"memory_entity_detail.html":    {"{{.Entity.ID}}": {ownRecord, 1}},
	"admin_blackbox_trigger.html":  {"{{.Trigger.ID}}": {ownRecord, 1}},
	"admin_workflow_proposal.html": {"{{.Proposal.ID}}": {ownRecord, 1}},
	"admin_chat_audit.html":        {"{{.SelectedDetail.ID}}": {ownRecord, 1}},
	"operator_regulatory.html":     {"{{.ID}}": {quotedOutside, 4}},
	"memory_project.html":          {"{{.ID}}": {dataNotText, 1}},
	"replay.html":                  {"{{.Timeline.Execution.ID}}": {dataNotText, 1}},
	"task_detail.html": {
		"{{.Project.ID}}": {chosenByAPerson, 3},
		// The heading (reason 2) and the page-data JSON (reason 4).
		"{{.Task.ID}}": {ownRecord, 2},
	},
	"task_live.html": {"{{.Execution.ID}}": {dataNotText, 1}, "{{.Task.ID}}": {dataNotText, 1}},
}

var bareAnyID854 = regexp.MustCompile(`\{\{-?\s*(?:\$[A-Za-z0-9_]*)?(?:\.[A-Za-z0-9_]+)*\.ID\s*-?\}\}`)

func TestShortIDs854_TheCensusCatchesEveryShape(t *testing.T) {
	for _, line := range []string{`<td>{{.ID}}</td>`, `<a href="/x">{{.Timeline.Task.ID}}</a>`, `{{$d.ID}}`} {
		if !bareAnyID854.MatchString(attr848.ReplaceAllString(line, "")) {
			t.Errorf("the census misses %q", line)
		}
	}
	for _, line := range []string{`<a href="/ui/tasks/{{.ID}}" title="{{.ID}}">{{shortID .ID}}</a>`, `<tr data-device="{{.ID}}">`, `{{.ProjectID}}`} {
		if bareAnyID854.MatchString(attr848.ReplaceAllString(line, "")) {
			t.Errorf("the census flags %q", line)
		}
	}
}

func TestShortIDs854_EveryIDPrintedAsTextIsADecision(t *testing.T) {
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	seen := map[string]map[string]int{}
	examined := 0
	for _, f := range files {
		b, err := fs.ReadFile(templatesFS, f)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(f, "templates/")
		for _, line := range strings.Split(string(b), "\n") {
			for _, m := range bareAnyID854.FindAllString(attr848.ReplaceAllString(line, ""), -1) {
				examined++
				if seen[name] == nil {
					seen[name] = map[string]int{}
				}
				seen[name][m]++
			}
		}
	}
	t.Logf("examined %d templates, %d IDs printed as text", len(files), examined)
	var bad []string
	for name, exprs := range seen {
		for expr, n := range exprs {
			want, ok := wholeIDs854[name][expr]
			switch {
			case !ok:
				bad = append(bad, name+": "+expr+" is printed whole and is not a §24 decision: wrap it in shortID with the full ID in title, or add it to wholeIDs854 with its reason")
			case n != want.count:
				bad = append(bad, name+": "+expr+" printed "+strconv.Itoa(n)+" times, the decision covers "+strconv.Itoa(want.count))
			}
		}
	}
	for name, exprs := range wholeIDs854 {
		for expr := range exprs {
			if seen[name][expr] == 0 {
				bad = append(bad, name+": "+expr+" is listed but no longer printed whole; remove it from wholeIDs854")
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d problems:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

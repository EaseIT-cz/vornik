package ui

// GitHub #848 (UI design §23): every visible task, execution and lease ID in
// a console template goes through shortID; the full ID stays in title and
// links. This census fails on a new page that prints the long form as text.
//
// Found at: production (operator, 2026-11-19: long IDs take a line of their
// own on a phone), observed at unit (the templates' text).

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Attributes (href, title, value, data-*) carry the full ID on purpose and
// are stripped first; what is left is text. A field named TaskID,
// ExecutionID, LeaseID or ParentTaskID on any receiver ({{.TaskID}},
// {{.Rating.ExecutionID}}, {{$d.ExecutionID}}) printed directly is a long ID
// as text; {{shortID ...}} does not match (review 9093 F1).
var (
	attr848       = regexp.MustCompile(`[A-Za-z_:-]+="[^"]*"`)
	bareIDText848 = regexp.MustCompile(`\{\{-?\s*(?:\$[A-Za-z0-9_]*)?(?:\.[A-Za-z0-9_]+)*\.(?:TaskID|ExecutionID|LeaseID|ParentTaskID)\s*-?\}\}`)
)

func TestShortIDs848_TheCensusCatchesEveryShape(t *testing.T) {
	for _, line := range []string{`<td>{{.TaskID}}</td>`, `<code>{{.Rating.ExecutionID}}</code>`, `({{$d.ExecutionID}})`, `exec={{.ExecutionID}}`} {
		if !bareIDText848.MatchString(attr848.ReplaceAllString(line, "")) {
			t.Errorf("the census misses %q", line)
		}
	}
	for _, line := range []string{`<a href="/ui/tasks/{{.TaskID}}" title="{{.TaskID}}">{{shortID .TaskID}}</a>`, `<input value="{{.ExecutionID}}">`} {
		if bareIDText848.MatchString(attr848.ReplaceAllString(line, "")) {
			t.Errorf("the census flags %q", line)
		}
	}
}

func TestShortIDs848_NoTemplatePrintsALongIDAsText(t *testing.T) {
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	var bad []string
	for _, f := range files {
		b, err := fs.ReadFile(templatesFS, f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bareIDText848.MatchString(attr848.ReplaceAllString(line, "")) {
				bad = append(bad, f+":"+strconv.Itoa(i+1))
			}
		}
	}
	t.Logf("examined %d templates", len(files))
	if len(bad) > 0 {
		t.Errorf("%d long IDs printed as text; wrap them in shortID and keep the full ID in title:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

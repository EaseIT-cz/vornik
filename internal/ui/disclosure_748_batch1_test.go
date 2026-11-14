package ui

// #748 rollout batch 1 (web-ui-design.md "Progressive disclosure across the
// console", E): task detail and task live use the shared disclosure, with
// every id, open condition and script hook kept. Found at: review (the #748
// audit, A: 16 own-style disclosures on these two pages); a user sees the
// rendered page, so the rendered checks sit at that level.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestBatch1_748_EveryDisclosureOnTheTaskPagesIsShared(t *testing.T) {
	detailsTag := regexp.MustCompile(`<details\b[^>]*>`)
	for _, f := range []string{"templates/task_detail.html", "templates/task_live.html"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		// Template markup only: drop <script> bodies and comments, which
		// mention <details> in prose.
		markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(src, "")
		tags := detailsTag.FindAllString(markup, -1)
		if len(tags) == 0 {
			t.Fatalf("%s: no <details> found; the check examined nothing", f)
		}
		classOf := regexp.MustCompile(`class="([^"]*)"`)
		for _, tag := range tags {
			m := classOf.FindStringSubmatch(tag)
			if m == nil || (m[1] != "vk-disc" && m[1] != "vk-disc vk-disc-row") {
				t.Errorf("%s: %s: a disclosure carries exactly the component's classes", f, tag)
			}
		}
		for _, sum := range regexp.MustCompile(`<summary\b[^>]*>`).FindAllString(markup, -1) {
			// An allowlist, not a denylist (review-20261113-67fe N1):
			// vk-disc-sum first, then only the layout classes a rich
			// summary's own content needs.
			m := classOf.FindStringSubmatch(sum)
			if m == nil {
				t.Errorf("%s: %s: a summary carries vk-disc-sum", f, sum)
				continue
			}
			toks := strings.Fields(m[1])
			if len(toks) == 0 || toks[0] != "vk-disc-sum" {
				t.Errorf("%s: %s: a summary starts with vk-disc-sum", f, sum)
				continue
			}
			for _, tok := range toks[1:] {
				if !summaryLayoutClasses[tok] {
					t.Errorf("%s: %s: %q is not a layout class a summary may add", f, sum, tok)
				}
			}
		}
		for _, old := range []string{"panel-ref", "group-open:"} {
			if strings.Contains(markup, old) {
				t.Errorf("%s still carries the per-page disclosure style %q", f, old)
			}
		}
		t.Logf("%s: %d disclosures examined", f, len(tags))
	}
}

func TestBatch1_748_TaskDetailKeepsItsOpenConditions(t *testing.T) {
	sectionTag := func(body, title string) string {
		i := strings.Index(body, ">"+title+"<")
		if i < 0 {
			return ""
		}
		j := strings.LastIndex(body[:i], "<details")
		if j < 0 {
			return ""
		}
		return body[j : j+strings.Index(body[j:], ">")+1]
	}
	completed := renderTaskDetailBody(t, TaskDetailData{
		Task:       &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCompleted, LeaseID: strPtr748("l1")},
		Execution:  &persistence.Execution{ID: "e1", Status: persistence.ExecutionStatusCompleted, Result: []byte(`{"message":"done"}`)},
		Executions: []*persistence.Execution{{ID: "e1", Status: persistence.ExecutionStatusCompleted}},
	})
	for _, c := range []struct {
		title string
		open  bool
	}{{"Output", true}, {"Lease", true}, {"Execution Attempts", true}} {
		tag := sectionTag(completed, c.title)
		if !strings.Contains(tag, "vk-disc") {
			t.Errorf("%s: not a shared disclosure: %q", c.title, tag)
			continue
		}
		if got := strings.Contains(tag, " open"); got != c.open {
			t.Errorf("%s for an admin on a completed task: open=%v, want %v (%s)", c.title, got, c.open, tag)
		}
	}
	roleUser := renderTaskDetailBody(t, TaskDetailData{
		Task:       &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, LeaseID: strPtr748("l1")},
		Executions: []*persistence.Execution{{ID: "e1", Status: persistence.ExecutionStatusRunning}},
		IsRoleUser: true,
	})
	for _, title := range []string{"Lease", "Execution Attempts", "Output"} {
		if tag := sectionTag(roleUser, title); strings.Contains(tag, " open") {
			t.Errorf("%s for a role user on a running task must stay closed: %s", title, tag)
		}
	}
}

func strPtr748(s string) *string { return &s }

// summaryLayoutClasses are the only classes a summary may add after
// vk-disc-sum: layout for a rich summary's own content.
var summaryLayoutClasses = map[string]bool{"text-sm": true, "flex-wrap": true, "gap-y-2": true}

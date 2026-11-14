package ui

// #748 batches 5 and 6 (web-ui-design.md "Progressive disclosure across the
// console", L): dispatcher tools, the project schema form and replay move
// their disclosures to the shared classes. Found at: review (the #748 audit).

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui/assetschema"
)

func TestBatch5_748_DisclosuresAreSharedAndClosed(t *testing.T) {
	classOf := regexp.MustCompile(`class="([^"]*)"`)
	for _, c := range []struct {
		file    string
		want    int
		mayOpen bool
	}{
		{"admin_integrations_dispatcher_tools.html", 1, false},
		{"asset_schema_config.html", 1, true},
		{"replay.html", 3, false},
	} {
		raw, err := os.ReadFile("templates/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		markup := regexp.MustCompile(`(?s)<script\b.*?</script>|<!--.*?-->|\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")
		tags := regexp.MustCompile(`<details\b[^>]*>`).FindAllString(markup, -1)
		if len(tags) != c.want {
			t.Errorf("%s: %d disclosures examined, want %d", c.file, len(tags), c.want)
		}
		for _, tag := range tags {
			if m := classOf.FindStringSubmatch(tag); m == nil || (m[1] != "vk-disc" && m[1] != "vk-disc vk-disc-row") {
				t.Errorf("%s: %s: a disclosure carries exactly the component's classes", c.file, tag)
			}
			if !c.mayOpen && strings.Contains(tag, " open") {
				t.Errorf("%s: %s renders open; it was closed by default", c.file, tag)
			}
		}
		for _, sum := range regexp.MustCompile(`<summary\b[^>]*>`).FindAllString(markup, -1) {
			m := classOf.FindStringSubmatch(sum)
			if m == nil {
				t.Errorf("%s: %s: a summary carries vk-disc-sum", c.file, sum)
				continue
			}
			toks := strings.Fields(m[1])
			if toks[0] != "vk-disc-sum" {
				t.Errorf("%s: %s: a summary starts with vk-disc-sum", c.file, sum)
			}
			for _, tok := range toks[1:] {
				if !summaryLayoutClasses[tok] {
					t.Errorf("%s: %s: %q is not a layout class a summary may add", c.file, sum, tok)
				}
			}
		}
		if strings.Contains(markup, "group-open:") {
			t.Errorf("%s still uses group-open:", c.file)
		}
	}
}

// A project schema section holding a field error opens, so a failed save
// shows its error (#748 L). By source: the open rule reads the section's
// fields' errors.
func TestBatch5_748_ProjectSchemaSectionOpensOnError(t *testing.T) {
	raw, err := os.ReadFile("templates/asset_schema_config.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// The open-on-error behaviour is checked rendered, below. Here: field
	// help stays a visible paragraph, each field showing once (design L;
	// review-20261113-80b4 asked for a positive check). Its class became the
	// caption step, text-xs, in #753 part B, replacing text-[11px].
	if !regexp.MustCompile(`<p class="text-xs text-gray-\d00[^"]*">\{\{\.Help\}\}</p>`).MatchString(src) {
		t.Error("the project schema form keeps its field help as a visible paragraph")
	}
}

// Rendered: an advanced section holding a field error opens; the other
// advanced sections stay closed.
func TestBatch5_748_ProjectSchemaRendersTheErrorSectionOpen(t *testing.T) {
	schema := assetschema.ProjectSchema()
	var advTitle, advPath, otherAdv string
	for _, sec := range schema.Sections {
		if !sec.Advanced || len(sec.Fields) == 0 {
			continue
		}
		if advTitle == "" {
			advTitle, advPath = sec.Title, sec.Fields[0].Path
		} else if otherAdv == "" {
			otherAdv = sec.Title
		}
	}
	if advTitle == "" || otherAdv == "" {
		t.Fatal("the project schema needs two advanced sections for this test")
	}
	body := renderTpl(t, "asset_schema_config.html", ProjectSchemaConfigData{
		Title: "Config", ProjectID: "p1", Schema: schema,
		Values: map[string]string{}, FieldErrors: map[string]string{advPath: "bad value"},
	})
	tagBefore := func(title string) string {
		i := strings.Index(body, ">"+title+"</h2>")
		if i < 0 {
			t.Fatalf("section %q not rendered", title)
		}
		tag := body[strings.LastIndex(body[:i], "<details"):i]
		gt := strings.Index(tag, ">")
		if gt < 0 {
			t.Fatalf("section %q: unterminated tag", title)
		}
		return tag[:gt]
	}
	if !strings.Contains(tagBefore(advTitle), " open") {
		t.Errorf("advanced section %q holds an error and must render open", advTitle)
	}
	if strings.Contains(tagBefore(otherAdv), " open") {
		t.Errorf("advanced section %q holds no error and must stay closed", otherAdv)
	}
}

// Execution's Associated Task keeps its "View task →" link after its summary
// moved from sectionHeader's Href to hand-written markup (#748 K;
// review-20261113-8a15).
func TestBatch4_748_ExecutionAssociatedTaskLinkSurvives(t *testing.T) {
	body := renderExecutionDetail(t, &persistence.Execution{ID: "exec-link", TaskID: "task-link", ProjectID: "p1", Status: persistence.ExecutionStatusCompleted})
	i := strings.Index(body, ">Associated Task</h2>")
	if i < 0 {
		t.Fatal("the Associated Task section is not rendered")
	}
	end := strings.Index(body[i:], "</summary>")
	if end < 0 || !strings.Contains(body[i:i+end], `href="/ui/tasks/task-link"`) || !strings.Contains(body[i:i+end], "View task →") {
		t.Errorf("the Associated Task summary keeps its task link; got %q", body[i:i+max(end, 0)])
	}
}

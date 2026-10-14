// Form-control boundaries: WCAG 2.1 SC 1.4.11 (non-text contrast)
// (https://docs.vornik.io §11.3).
//
// audit 2026-10-12 (GitHub #164): inputs, selects and textareas drew their
// boundary with surface-700/600, 1.00–1.47:1 against the surfaces they sit on.
// The palette test pins --control-border at 3:1 or better; this test pins
// that every visible control actually USES it, and publishes the denominator
// (examined / migrated / exempt by reason), so "clean" cannot mean "never
// looked at".
package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	ctlTag        = regexp.MustCompile(`(?is)<(input|select|textarea)\b([^>]*)>`)
	ctlClass      = regexp.MustCompile(`(?s)\bclass="([^"]*)"`)
	ctlType       = regexp.MustCompile(`\btype="([^"]*)"`)
	ctlID         = regexp.MustCompile(`\bid="([^"]*)"`)
	ctlDisabled   = regexp.MustCompile(`(?:^|\s)disabled(?:\s|$|=)`)
	tplComment    = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	htmlComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	scriptBody    = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	borderWidth   = regexp.MustCompile(`(?:^|\s)border(?:-[0-9]|-[xytblr](?:-[0-9])?)?(?:\s|$)`)
	ringWidth     = regexp.MustCompile(`(?:^|\s)ring(?:-[0-9])?(?:\s|$)`)
	baseBorderCol = regexp.MustCompile(`(?:^|\s)border-(?:dark|surface|ink|gray)-[0-9]+(?:/[0-9]+)?(?:\s|$)`)
)

// wrappedControls are borderless fields whose wrapper carries the boundary
// (design §11.3 item 3). The test checks the wrapper, not just the name.
var wrappedControls = map[string]bool{
	"tasks.html#task-filter":       true,
	"swarms.html#ia-filter":        true,
	"workflows.html#ia-filter":     true,
	"_partials.html#palette-input": true,
}

// stripNonMarkup blanks template comments, HTML comments and script bodies,
// keeping their newlines so line numbers still point at the file.
func stripNonMarkup(src string) string {
	keepLines := func(s string) string { return strings.Repeat("\n", strings.Count(s, "\n")) }
	for _, re := range []*regexp.Regexp{tplComment, htmlComment, scriptBody} {
		src = re.ReplaceAllStringFunc(src, keepLines)
	}
	return src
}

func TestControls_UseControlBorderToken(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	tally := map[string]int{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := stripNonMarkup(string(raw))
		base := filepath.Base(f)
		for _, m := range ctlTag.FindAllStringSubmatchIndex(src, -1) {
			tag := strings.ToLower(src[m[2]:m[3]])
			attrs := src[m[4]:m[5]]
			line := strings.Count(src[:m[0]], "\n") + 1
			where := base + ":" + strconv.Itoa(line)
			typ := tag
			if tag == "input" {
				typ = "text"
				if tm := ctlType.FindStringSubmatch(attrs); tm != nil {
					typ = tm[1]
				}
			}
			if typ == "hidden" {
				tally["hidden (not a control)"]++
				continue
			}
			tally["examined"]++
			cls := ""
			if cm := ctlClass.FindStringSubmatch(attrs); cm != nil {
				cls = " " + cm[1] + " "
			}
			hasBorder := strings.Contains(cls, " border-control ") && borderWidth.MatchString(cls)
			hasRing := strings.Contains(cls, " ring-control ") && ringWidth.MatchString(cls)
			hasToken := hasBorder || hasRing
			if baseBorderCol.MatchString(cls) {
				t.Errorf("%s: <%s type=%s> still draws its boundary with a surface/ink border class: %s", where, tag, typ, strings.TrimSpace(cls))
				continue
			}
			switch {
			case typ == "checkbox" || typ == "radio":
				if strings.Contains(cls, " border-control ") {
					tally["exempt: native checkbox/radio (class on border-control)"]++
				} else {
					tally["exempt: native checkbox/radio (no border class)"]++
				}
				continue
			case strings.Contains(cls, " sr-only "):
				tally["exempt: sr-only (visible proxy is a toggle group)"]++
				continue
			case ctlDisabled.MatchString(attrs) && !hasToken:
				tally["exempt: disabled (inactive component)"]++
				continue
			}
			if id := ctlID.FindStringSubmatch(attrs); id != nil && wrappedControls[base+"#"+id[1]] {
				lo := m[0] - 1200
				if lo < 0 {
					lo = 0
				}
				if !strings.Contains(src[lo:m[0]], "border-control") {
					t.Errorf("%s: wrapped control #%s: no border-control on its wrapper", where, id[1])
					continue
				}
				tally["exempt: wrapper carries border-control"]++
				continue
			}
			if !hasToken {
				t.Errorf("%s: <%s type=%s> has no `border border-control` (or `ring-1 ring-control`) boundary: %q", where, tag, typ, strings.TrimSpace(cls))
				continue
			}
			tally["migrated: border-control"]++
		}
	}
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-52s %d", k, tally[k])
	}
	if tally["examined"] < 250 {
		t.Errorf("examined only %d visible controls; the scan has gone blind (design §11.3 counted 294 in internal/ui/templates)", tally["examined"])
	}
}

// Toggle groups and pressed-state buttons are 1.4.11 components too.
func TestControls_ToggleGroupsUseControlBorder(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	group := regexp.MustCompile(`<div\b[^>]*\brole="group"[^>]*>`)
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range group.FindAllString(string(raw), -1) {
			n++
			if !strings.Contains(g, "border-control") {
				t.Errorf("%s: toggle group without border-control: %s", filepath.Base(f), g)
			}
		}
	}
	if n < 4 {
		t.Errorf("found %d role=group toggle containers, want >= 4 (dashboard, spend, trading, tasks view)", n)
	}
	td, err := os.ReadFile("templates/task_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(string(td), "{{else}}border-control text-ink-300{{end}}"); c != 2 {
		t.Errorf("task_detail rating buttons: %d resting borders on border-control, want 2", c)
	}
	live, err := os.ReadFile("templates/task_live.html")
	if err != nil {
		t.Fatal(err)
	}
	chip := string(live)
	if i := strings.Index(chip, ".scope-chip {"); i < 0 || !strings.Contains(chip[i:i+300], "border-color: rgb(var(--control-border))") {
		t.Error("task_live .scope-chip border does not use --control-border")
	}
}

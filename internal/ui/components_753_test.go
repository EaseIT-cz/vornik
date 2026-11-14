package ui

// Design system v2 part C, components (ui-design-refresh-design.md §12.2 C,
// #753). Before #753 the console had no shared button or card: 251 buttons
// carried 123 distinct class strings and radii came in six sizes.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// componentCSS returns the component block of _partials.html.
func componentCSS(t *testing.T) string {
	t.Helper()
	src := partials(t)
	const start, end = "/* === Components (#753 part C) === */", "/* === end components === */"
	i, j := strings.Index(src, start), strings.Index(src, end)
	if i < 0 || j < i {
		t.Fatalf("component block %q … %q not found in _partials.html", start, end)
	}
	return src[i:j]
}

func TestComponents_ClassesExist_753(t *testing.T) {
	css := componentCSS(t)
	for _, c := range []string{
		".v-btn", ".v-btn--primary", ".v-btn--secondary", ".v-btn--quiet", ".v-btn--danger",
		".v-card", ".v-card--raised", ".v-list", ".v-list-row", ".v-inbox-row",
		".v-dot", ".v-dot--warn", ".v-dot--ok", ".v-dot--danger", ".v-badge",
		".v-stat", ".v-empty", ".v-field", ".v-field__control", ".v-field__error",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(c) + `[\s,{:.]`).MatchString(css) {
			t.Errorf("component class %s is not defined", c)
		}
	}
}

// Components read tokens only: no hex colour, and only the three radii.
func TestComponents_TokenOnly_753(t *testing.T) {
	// Comments cite issues (#753), which look like hex colours: strip them.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(componentCSS(t), "")
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).FindString(css); m != "" {
		t.Errorf("component CSS uses the literal colour %s; read a token", m)
	}
	for _, m := range regexp.MustCompile(`border-radius:\s*([^;]+);`).FindAllStringSubmatch(css, -1) {
		switch strings.TrimSpace(m[1]) {
		case "12px", "8px", "9999px", "0":
		default:
			t.Errorf("component radius %s; the scale is 12px, 8px and 9999px", m[1])
		}
	}
	for _, sel := range []string{`\.v-btn`, `\.v-field__control`} {
		rule := regexp.MustCompile(sel + `\s*\{[^}]*\}`).FindString(css)
		if !strings.Contains(rule, "min-height: 44px") {
			t.Errorf("%s lacks min-height: 44px (touch target): %s", sel, rule)
		}
	}
}

// rawButtonCeiling is the number of <button> elements in the console
// templates that do not use .v-btn. It may only go down: pages adopt the
// component one by one (#761), and a new raw button fails here. A count,
// not a per-button allowlist: simpler, at the cost that deleting a button
// also lowers it (design §12.2 C, as shipped).
const rawButtonCeiling = 203

func TestComponents_ButtonAdoptionRatchet_753(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	tag := regexp.MustCompile(`<button\b[^>]*>`)
	usesBtn := regexp.MustCompile(`\bv-btn\b`)
	total, raw := 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tag.FindAllString(string(b), -1) {
			total++
			if !usesBtn.MatchString(m) {
				raw++
			}
		}
	}
	t.Logf("examined %d <button> elements in %d templates: %d use .v-btn, %d raw (ceiling %d)", total, len(files), total-raw, raw, rawButtonCeiling)
	if total < 200 {
		t.Errorf("examined only %d buttons; the glob no longer matches the console", total)
	}
	if raw > rawButtonCeiling {
		t.Errorf("%d raw buttons, ceiling %d: use the .v-btn component", raw, rawButtonCeiling)
	}
	if raw < rawButtonCeiling {
		t.Logf("raw buttons fell to %d: lower rawButtonCeiling to lock it in", raw)
	}
}

// The 8 files #759 converted (design §17.2 M2) keep no raw button: the
// global ceiling alone could be met by converting other pages while these
// stay raw. Same two patterns as the ratchet.
func TestComponents_OwnerPagesUseTheButton_759(t *testing.T) {
	tag := regexp.MustCompile(`<button\b[^>]*>`)
	usesBtn := regexp.MustCompile(`\bv-btn\b`)
	for _, f := range []string{"inbox", "project_detail", "project_keys", "reminders", "my_account", "integrations_form", "admin_credentials", "companion"} {
		b, err := os.ReadFile("templates/" + f + ".html")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tag.FindAllString(string(b), -1) {
			if !usesBtn.MatchString(m) {
				t.Errorf("%s.html has a raw button: %s", f, m)
			}
		}
	}
}

// Every inbox row kind is a .v-inbox-row, with its id unchanged (#759 M2).
func TestInbox_RowsUseTheComponent_759(t *testing.T) {
	b, err := os.ReadFile("templates/inbox.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `class="rounded-xl border border-dark-600 bg-dark-800`) {
		t.Error("an inbox row still uses the hand-styled card")
	}
	if n := strings.Count(s, `class="v-inbox-row"><div class="v-inbox-row__body`); n != 6 {
		t.Errorf("%d rows use .v-inbox-row, want the 6 row kinds", n)
	}
	for _, id := range []string{`id="inbox-item-{{.TaskID}}" class="v-inbox-row"`, `id="broker-action-{{.ActionID}}" class="v-inbox-row"`, `id="{{.Anchor}}" class="v-inbox-row"`} {
		if !strings.Contains(s, id) {
			t.Errorf("row id lost: %s", id)
		}
	}
}

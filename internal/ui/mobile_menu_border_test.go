// Phone menus draw one edge, the top (operator report, 2026-11-02: "borders
// are visible now on the bottom menu").
//
// Found at: production (operator's phone, dark theme). The bottom tab bar and
// its drawers carry `glass menu-solid border-t`. `.glass` draws a 1px border
// on all four sides (rgba(255,255,255,0.22) in dark), so the bar was boxed:
// left, right and bottom edges against the screen. `border-t` only asks for
// the top one. `.menu-solid`, which only these two surfaces use, now drops the
// other three. Design: ui-phone-first-design.md §3.1, amendment 2026-11-02.
//
// Regression test at: unit (the stylesheet rule and the markup that uses it).
// Lower because: CI has no browser lane that renders the console at phone
// width; the fault is one CSS rule, and this test fails on the old rule.
package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestMobileMenus_DrawOnlyTheTopBorder(t *testing.T) {
	b, err := os.ReadFile("templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	rule := regexp.MustCompile(`(?s)\.menu-solid\s*\{([^}]*)\}`).FindStringSubmatch(src)
	if rule == nil {
		t.Fatal("no .menu-solid rule in _partials.html")
	}
	body := strings.Join(strings.Fields(rule[1]), " ")
	for _, want := range []string{"border-right-width: 0", "border-bottom-width: 0", "border-left-width: 0"} {
		if !strings.Contains(body, want) {
			t.Errorf(".menu-solid lacks %q, so the .glass box border shows on the phone menus: { %s }", want, body)
		}
	}
	if strings.Contains(body, "border-top-width: 0") || strings.Contains(body, "border: 0") || strings.Contains(body, "border: none") {
		t.Errorf(".menu-solid removes the top border too: { %s }", body)
	}

	// The denominator: every element that uses menu-solid is a phone menu
	// that asks for the top border, so dropping the other three is right
	// for each of them.
	uses := regexp.MustCompile(`class="([^"]*\bmenu-solid\b[^"]*)"`).FindAllStringSubmatch(src, -1)
	if len(uses) != 2 {
		t.Fatalf("menu-solid used %d times, want 2 (the bottom bar and its drawers)", len(uses))
	}
	for _, u := range uses {
		if !regexp.MustCompile(`(?:^|\s)border-t(?:\s|$)`).MatchString(u[1]) {
			t.Errorf("a menu-solid element does not ask for border-t: %q", u[1])
		}
	}
}

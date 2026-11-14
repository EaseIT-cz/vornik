// Accessibility follow-ups from #164 (GitHub #335; design
// https://docs.vornik.io §11.4): the dark glass
// fallback, the live-task scope-chip text, and icon-only controls.
package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var themeHeaders = map[string]string{
	"light": `:root, [data-theme="light"] {`,
	"dark":  `[data-theme="dark"] {`,
}

// cssColour resolves one CSS colour value against a theme's tokens:
// `rgb(var(--x))`, `rgb(var(--x) / a)`, `rgb(r g b)`, `rgb(r g b / a)` or
// `white`. It returns the colour and its alpha.
func cssColour(t *testing.T, v string, m map[string]rgb) (rgb, float64) {
	t.Helper()
	v = strings.TrimSpace(v)
	if v == "white" || v == "#fff" || v == "#FFF" {
		return white, 1
	}
	if mm := regexp.MustCompile(`^rgb\(var\(--([a-z0-9-]+)\)\s*(?:/\s*([0-9.]+))?\)$`).FindStringSubmatch(v); mm != nil {
		c, ok := m[mm[1]]
		if !ok {
			t.Fatalf("unknown token --%s in %q", mm[1], v)
		}
		return c, alpha(t, mm[2])
	}
	if mm := regexp.MustCompile(`^rgb\((\d+)\s+(\d+)\s+(\d+)\s*(?:/\s*([0-9.]+))?\)$`).FindStringSubmatch(v); mm != nil {
		var c rgb
		if _, err := fmt.Sscanf(mm[1]+" "+mm[2]+" "+mm[3], "%f %f %f", &c.r, &c.g, &c.b); err != nil {
			t.Fatal(err)
		}
		return c, alpha(t, mm[4])
	}
	t.Fatalf("cannot resolve colour %q", v)
	return rgb{}, 0
}

func alpha(t *testing.T, s string) float64 {
	t.Helper()
	if s == "" {
		return 1
	}
	var a float64
	if _, err := fmt.Sscanf(s, "%f", &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// cssDecl returns the value of prop in the first rule whose selector line is
// exactly sel (e.g. ".scope-chip {"), or "" when the rule does not set it.
func cssDecl(t *testing.T, src, sel, prop string) string {
	t.Helper()
	i := strings.Index(src, sel)
	if i < 0 {
		t.Fatalf("rule %q not found", sel)
	}
	body := src[i+len(sel):]
	end := strings.Index(body, "}")
	if end < 0 {
		t.Fatalf("rule %q is not closed", sel)
	}
	body = body[:end]
	mm := regexp.MustCompile(`(?m)(?:^|[;{\s])` + regexp.QuoteMeta(prop) + `:\s*([^;]+);`).FindStringSubmatch(body)
	if mm == nil {
		return ""
	}
	// Drop a trailing /* comment */.
	return strings.TrimSpace(regexp.MustCompile(`/\*.*?\*/`).ReplaceAllString(mm[1], ""))
}

// Design system v2 part D (ui-design-refresh-design.md §12.2 D, #753):
// .glass is the opaque card. Before #753 it was translucent white at 0.60
// (light) and 0.12 (dark) over a 12px backdrop blur, with two fallbacks
// (no backdrop-filter, reduced transparency) that GitHub #335 item 1 had to
// repair. With an opaque surface the fallbacks have nothing to do, so they
// are gone, and every pair pinned on surface-800 holds on glass by
// construction.
func TestPalette_GlassIsFlat_753(t *testing.T) {
	src := partials(t)
	for _, gone := range []string{"@supports not (backdrop-filter", "prefers-reduced-transparency", "backdrop-filter: blur"} {
		if strings.Contains(src, gone) {
			t.Errorf("_partials.html still has %q; .glass is opaque", gone)
		}
	}
	rule := regexp.MustCompile(`\.glass\s*\{[^}]*\}`).FindString(src)
	if rule == "" {
		t.Fatal(".glass rule not found")
	}
	if strings.Contains(rule, "box-shadow") || strings.Contains(rule, "backdrop") {
		t.Errorf(".glass keeps a shadow or blur: %s", rule)
	}
	for theme, header := range themeHeaders {
		m := themeTokens(t, src, header)
		gbg, ga := glass(t, src, header)
		if ga != 1 || gbg != m["surface-800"] {
			t.Errorf("%s: glass fill %v at %.2f, want surface-800 %v at 1", theme, gbg, ga, m["surface-800"])
		}
	}
	// The border is the card hairline in both themes (it was translucent
	// white before #753, invisible on the opaque light card).
	g := strings.Index(src, "Glass surface system")
	if n := strings.Count(src[g:], "--glass-border: rgb(var(--surface-600));"); n != 2 {
		t.Errorf("--glass-border should be rgb(var(--surface-600)) in both themes; found %d", n)
	}
}

// No template may still read a glass token #753 part D deleted: the
// declaration would be silently invalid.
func TestPalette_NoDeletedGlassTokens_753(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`var\(--glass-(blur|shadow|inner)\)`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.FindString(string(raw)); m != "" {
			t.Errorf("%s reads %s, deleted in #753 part D", f, m)
		}
	}
	t.Logf("examined %d templates", len(files))
}

// glassComposites returns .glass over page and over card for a theme.
func glassComposites(t *testing.T, src, header string, m map[string]rgb) map[string]rgb {
	t.Helper()
	gbg, ga := glass(t, src, header)
	return map[string]rgb{
		"glass over page": over(gbg, m["surface-900"], ga),
		"glass over card": over(gbg, m["surface-800"], ga),
	}
}

// audit 2026-10-12 (GitHub #335 item 2): the steering scope chips hard-coded
// Tailwind gray/sky colours that bypass the theme tokens; in light all three
// states failed AA (inactive 2.45, hover 1.42, active 1.53 on the glass
// aside). 10px text: the 4.5:1 normal-text threshold applies.
func TestPalette_ScopeChipTextContrast(t *testing.T) {
	src := partials(t)
	raw, err := os.ReadFile("templates/task_live.html")
	if err != nil {
		t.Fatal(err)
	}
	live := string(raw)
	baseBG := cssDecl(t, live, ".scope-chip {", "background-color")
	states := []struct{ name, sel string }{
		{"inactive", ".scope-chip {"},
		{"hover", ".scope-chip:hover {"},
		{"active", ".scope-chip.active {"},
	}
	for theme, header := range themeHeaders {
		m := themeTokens(t, src, header)
		for bn, backdrop := range glassComposites(t, src, header, m) {
			for _, s := range states {
				fg := cssDecl(t, live, s.sel, "color")
				if fg == "" {
					t.Fatalf("%s chip sets no color", s.name)
				}
				bg := cssDecl(t, live, s.sel, "background-color")
				if bg == "" {
					bg = baseBG
				}
				fc, _ := cssColour(t, fg, m)
				bc, ba := cssColour(t, bg, m)
				fill := over(bc, backdrop, ba)
				got := contrast(fc, fill)
				if got < 4.5 {
					t.Errorf("%s: %s scope chip text on %s = %.2f:1, want >= 4.5:1", theme, s.name, bn, got)
					continue
				}
				t.Logf("%s: %s scope chip on %s = %.2f:1", theme, s.name, bn, got)
				// The chip's boundary (SC 1.4.11, 3:1) in every state
				// (review-20261012-057d F3: the brand-500 hover/active
				// border was 2.15–2.39:1 on dark glass).
				border := cssDecl(t, live, s.sel, "border-color")
				if border == "" {
					t.Fatalf("%s chip sets no border-color", s.name)
				}
				bdc, bda := cssColour(t, border, m)
				if got := contrast(over(bdc, backdrop, bda), backdrop); got < 3 {
					t.Errorf("%s: %s scope chip border on %s = %.2f:1, want >= 3:1", theme, s.name, bn, got)
				}
			}
		}
	}
}

package ui

// Design system v2 part C, status colours (ui-design-refresh-design.md
// §12.2 C, #753). Before #753 the status families (rose, red, amber,
// yellow, emerald, green; about 2,300 class uses) were Tailwind's own
// colours, untokenised and unpinned: white on emerald-500 was 2.5:1.
// They now alias three brand ramps, danger, warn and ok, that stay ordered
// by lightness in both themes, so existing `text-rose-700
// dark:text-rose-300` pairs keep their meaning.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const statusRampHeader = ":root { /* status ramps (#753 part C) */"

var statusSteps = []string{"50", "100", "200", "300", "400", "500", "600", "700", "800", "900", "950"}

func TestStatusRamps_Contrast_753(t *testing.T) {
	src := partials(t)
	ramp := themeTokens(t, src, statusRampHeader)
	light := themeTokens(t, src, `:root, [data-theme="light"] {`)
	dark := themeTokens(t, src, `[data-theme="dark"] {`)
	checks := 0
	for _, fam := range []string{"danger", "warn", "ok"} {
		get := func(step string) rgb {
			c, ok := ramp[fam+"-"+step]
			if !ok {
				t.Fatalf("--%s-%s missing from the status ramp block", fam, step)
			}
			return c
		}
		// Ordered by lightness, so the class numbers keep their meaning.
		for i := 0; i+1 < len(statusSteps); i++ {
			if get(statusSteps[i]).lum() <= get(statusSteps[i+1]).lum() {
				t.Errorf("--%s-%s is not lighter than --%s-%s; the ramp must get darker with every step", fam, statusSteps[i], fam, statusSteps[i+1])
			}
		}
		for _, s := range []string{"600", "700", "800"} {
			for _, g := range []string{"surface-800", "surface-900"} {
				checks++
				if c := contrast(get(s), light[g]); c < 4.5 {
					t.Errorf("light: %s-%s text on %s is %.2f:1, want 4.5", fam, s, g, c)
				}
			}
			checks++
			if c := contrast(white, get(s)); c < 4.5 {
				t.Errorf("white on %s-%s is %.2f:1, want 4.5", fam, s, c)
			}
		}
		for _, s := range []string{"200", "300", "400"} {
			for _, g := range []string{"surface-800", "surface-900"} {
				checks++
				if c := contrast(get(s), dark[g]); c < 4.5 {
					t.Errorf("dark: %s-%s text on %s is %.2f:1, want 4.5", fam, s, g, c)
				}
			}
		}
	}
	t.Logf("checked %d status ramp pairs", checks)
}

// The semantic tokens the components read, per theme.
func TestStatusSemanticTokens_753(t *testing.T) {
	src := partials(t)
	for _, th := range []struct{ name, header string }{
		{"light", `:root, [data-theme="light"] {`},
		{"dark", `[data-theme="dark"] {`},
	} {
		m := themeTokens(t, src, th.header)
		for _, fam := range []string{"danger", "warn", "ok"} {
			txt, fill, tnt := m[fam+"-text"], m[fam+"-fill"], m[fam+"-tint"]
			if txt == (rgb{}) || fill == (rgb{}) || tnt == (rgb{}) {
				t.Fatalf("%s: --%s-text, -fill or -tint missing", th.name, fam)
			}
			for _, g := range []string{"surface-800", "surface-900"} {
				if c := contrast(txt, m[g]); c < 4.5 {
					t.Errorf("%s: --%s-text on %s is %.2f:1, want 4.5", th.name, fam, g, c)
				}
			}
			if c := contrast(white, fill); c < 4.5 {
				t.Errorf("%s: white on --%s-fill is %.2f:1, want 4.5", th.name, fam, c)
			}
			if c := contrast(txt, tnt); c < 4.5 {
				t.Errorf("%s: --%s-text on --%s-tint is %.2f:1, want 4.5", th.name, fam, fam, c)
			}
			// Each role is a ramp step, so a ramp edit cannot strand it.
			ramp := themeTokens(t, src, statusRampHeader)
			for role, c := range map[string]rgb{"text": txt, "fill": fill, "tint": tnt} {
				found := false
				for _, st := range statusSteps {
					if ramp[fam+"-"+st] == c {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: --%s-%s %v is not a step of the %s ramp", th.name, fam, role, c, fam)
				}
			}
		}
		// The secondary button's hover: ink-100 on surface-700.
		if c := contrast(m["ink-100"], m["surface-700"]); c < 4.5 {
			t.Errorf("%s: ink-100 on surface-700 (secondary button hover) is %.2f:1, want 4.5", th.name, c)
		}
	}
}

func TestStatusFamiliesAliasTheRamps_753(t *testing.T) {
	raw, err := os.ReadFile("tailwind/tailwind.config.js")
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	for fam, ramp := range map[string]string{"rose": "danger", "red": "danger", "amber": "warn", "yellow": "warn", "emerald": "ok", "green": "ok"} {
		if !regexp.MustCompile(fam + `:\s*` + ramp + `Ramp`).MatchString(cfg) {
			t.Errorf("tailwind.config.js does not alias %s onto the %s ramp", fam, ramp)
		}
	}
	css, err := os.ReadFile("static/tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rgb(var(--danger-700)", "rgb(var(--warn-700)", "rgb(var(--ok-700)", "rgb(var(--danger-300)", "rgb(var(--ok-300)"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("static/tailwind.css never reads %s (run make ui-css)", want)
		}
	}
}

// White text on a 300, 400 or 500 status fill fails AA (3.9:1 at best);
// fills behind white text use 600 or darker. Alpha variants count.
// Widened from 500 only after review-20261113-ea4c.
func TestNoWhiteOnStatus500_753(t *testing.T) {
	cls := regexp.MustCompile(`class="([^"]*)"`)
	bad := regexp.MustCompile(`(^|\s)bg-(rose|red|amber|yellow|emerald|green)-[345]00(/\d+)?(\s|$)`)
	for _, f := range typeScaleEmitters(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cls.FindAllStringSubmatch(string(raw), -1) {
			if strings.Contains(" "+m[1]+" ", " text-white ") && bad.MatchString(m[1]) {
				t.Errorf("%s: white text on a 500 status fill: %q", f, m[1])
			}
		}
	}
}

// Found in the part C review (review-20261113-ea4c): 53 status texts in
// admin pages, the MCP probe and the task log used a dark-theme step
// (text-amber-300, text-red-400, text-rose-500) with no light-theme
// partner, so in the light theme, now the default, every one failed AA on a
// light surface (1.4 to 3.9:1). A status text below 600 must carry a dark:
// partner and a light step of 600 or darker.
func TestStatusText_HasALightStep_753(t *testing.T) {
	cls := regexp.MustCompile(`class="([^"]*)"`)
	bare := regexp.MustCompile(`(^|\s)text-(rose|red|amber|yellow|emerald|green)-(50|100|200|300|400|500)(\s|$)`)
	examined, bad := 0, 0
	for _, f := range typeScaleEmitters(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cls.FindAllStringSubmatch(string(raw), -1) {
			examined++
			if bare.MatchString(m[1]) {
				bad++
				if bad <= 10 {
					t.Errorf("%s: %q has a dark-theme status step with no light-theme step", f, m[1])
				}
			}
		}
	}
	t.Logf("examined %d class attributes; %d light-theme status texts below AA", examined, bad)
	if bad > 10 {
		t.Errorf("... and %d more", bad-10)
	}
}

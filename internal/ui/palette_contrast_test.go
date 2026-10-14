// UI palette contrast: Brand Manual v1.2 tokens
// (https://docs.vornik.io §11.2).
//
// Before this test the light theme's link colour was 2.5–2.8:1 on its own
// surfaces and the dark theme's 2.5:1 on cards (§11.1): nothing measured the
// token pairs the templates actually combine. These checks compute WCAG 2.x
// contrast from the token blocks in _partials.html, per theme.
package ui

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

type rgb struct{ r, g, b float64 }

func (c rgb) lum() float64 {
	ch := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.r) + 0.7152*ch(c.g) + 0.0722*ch(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := a.lum(), b.lum()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// over composites fg at alpha a onto bg.
func over(fg, bg rgb, a float64) rgb {
	return rgb{a*fg.r + (1-a)*bg.r, a*fg.g + (1-a)*bg.g, a*fg.b + (1-a)*bg.b}
}

var white = rgb{255, 255, 255}

// themeTokens parses `--name: R G B;` lines from the block opened by header.
func themeTokens(t *testing.T, src, header string) map[string]rgb {
	t.Helper()
	i := strings.Index(src, header)
	if i < 0 {
		t.Fatalf("token block %q not found", header)
	}
	j := strings.Index(src[i:], "}")
	out := map[string]rgb{}
	re := regexp.MustCompile(`--([a-z0-9-]+):\s*(\d+)\s+(\d+)\s+(\d+);`)
	for _, m := range re.FindAllStringSubmatch(src[i:i+j], -1) {
		var c rgb
		if _, err := fmt.Sscanf(m[2]+" "+m[3]+" "+m[4], "%f %f %f", &c.r, &c.g, &c.b); err != nil {
			t.Fatalf("token --%s: %v", m[1], err)
		}
		out[m[1]] = c
	}
	return out
}

func partials(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type pair struct {
	name     string
	fg, bg   func(map[string]rgb) rgb
	min      float64
	onlyThem string // "" = both themes
}

func tok(n string) func(map[string]rgb) rgb { return func(m map[string]rgb) rgb { return m[n] } }
func lit(c rgb) func(map[string]rgb) rgb    { return func(map[string]rgb) rgb { return c } }
func tint(n string, a float64, bg string) func(map[string]rgb) rgb {
	return func(m map[string]rgb) rgb { return over(m[n], m[bg], a) }
}

func TestPalette_ContrastPerTheme(t *testing.T) {
	src := partials(t)
	themes := map[string]map[string]rgb{
		"light": themeTokens(t, src, `:root, [data-theme="light"] {`),
		"dark":  themeTokens(t, src, `[data-theme="dark"] {`),
	}
	var pairs []pair
	for _, s := range []string{"surface-800", "surface-900"} {
		for _, b := range []string{"brand-200", "brand-300", "brand-400"} {
			pairs = append(pairs, pair{b + " text on " + s, tok(b), tok(s), 4.5, ""})
		}
		for _, i := range []string{"ink-100", "ink-200", "ink-300", "ink-400", "ink-500"} {
			pairs = append(pairs, pair{i + " text on " + s, tok(i), tok(s), 4.5, ""})
		}
		pairs = append(pairs, pair{"ink-600 (disabled) on " + s, tok("ink-600"), tok(s), 3, ""})
		pairs = append(pairs,
			pair{"brand-200 on brand-900/20 over " + s, tok("brand-200"), tint("brand-900", .2, s), 4.5, ""},
			pair{"brand-50 on brand-900/40 over " + s, tok("brand-50"), tint("brand-900", .4, s), 4.5, ""},
			pair{"focus ring brand-500 vs " + s, tok("brand-500"), tok(s), 3, ""},
		)
	}
	pairs = append(pairs,
		pair{"accent-300 text on surface-800", tok("accent-300"), tok("surface-800"), 4.5, ""},
		pair{"accent-400 text on surface-800", tok("accent-400"), tok("surface-800"), 4.5, ""},
		pair{"accent-300 on accent-900/50 over surface-900", tok("accent-300"), tint("accent-900", .5, "surface-900"), 4.5, ""},
		pair{"white on brand-500", lit(white), tok("brand-500"), 4.5, ""},
		pair{"white on brand-600", lit(white), tok("brand-600"), 4.5, ""},
		pair{"white on brand-400", lit(white), tok("brand-400"), 4.5, "light"},
		pair{"white on accent-500", lit(white), tok("accent-500"), 4.5, ""},
		pair{"white on accent-600", lit(white), tok("accent-600"), 4.5, ""},
	)
	for name, m := range themes {
		for _, p := range pairs {
			if p.onlyThem != "" && p.onlyThem != name {
				continue
			}
			if got := contrast(p.fg(m), p.bg(m)); got < p.min {
				t.Errorf("%s: %s = %.2f:1, want >= %.1f:1", name, p.name, got, p.min)
			}
		}
	}
}

// Dark brand-400 is link text and too light to sit behind white text, so no
// template may use it as a fill (design §11.2 Decision 5).
func TestPalette_NoWhiteTextOnBrand400(t *testing.T) {
	// #334 item 6 (audit 2026-10-12): the ReadDir error was discarded, so a
	// moved templates/ directory checked nothing and passed.
	files, err := os.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	t.Logf("brand-400 law: examined %d template file(s)", len(files))
	if len(files) == 0 {
		t.Fatal("brand-400 law: examined 0 templates; a law that looked at nothing has not passed")
	}
	re := regexp.MustCompile(`class="[^"]*\bbg-brand-400\b[^"]*"`)
	for _, f := range files {
		raw, err := os.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "hover:bg-brand-400") {
			t.Errorf("%s: hover:bg-brand-400 behind white text (use hover:bg-brand-500)", f.Name())
		}
		for _, m := range re.FindAllString(string(raw), -1) {
			if strings.Contains(m, "text-white") {
				t.Errorf("%s: bg-brand-400 with text-white: %s", f.Name(), m)
			}
		}
	}
}

// glass returns the .glass fill (--glass-bg at --glass-bg-alpha) per theme,
// parsed from the glass token blocks that follow the palette blocks.
func glass(t *testing.T, src, header string) (rgb, float64) {
	t.Helper()
	g := strings.Index(src, "Glass surface system")
	if g < 0 {
		t.Fatal("glass token block not found")
	}
	toks := themeTokens(t, src[g:], header)
	i := strings.Index(src[g:], header)
	m := regexp.MustCompile(`--glass-bg-alpha:\s*([0-9.]+);`).FindStringSubmatch(src[g+i:])
	if m == nil {
		t.Fatalf("no --glass-bg-alpha after %q", header)
	}
	var a float64
	if _, err := fmt.Sscanf(m[1], "%f", &a); err != nil {
		t.Fatal(err)
	}
	return toks["glass-bg"], a
}

// audit 2026-10-12 (GitHub #164): form-control boundaries were drawn with
// surface-700/600 (1.00–1.47:1), under WCAG 1.4.11's 3:1. --control-border
// must clear 3:1 against every surface a control sits on, glass included
// (design §11.3).
func TestPalette_ControlBorderNonTextContrast(t *testing.T) {
	src := partials(t)
	for name, header := range map[string]string{
		"light": `:root, [data-theme="light"] {`,
		"dark":  `[data-theme="dark"] {`,
	} {
		m := themeTokens(t, src, header)
		cb, ok := m["control-border"]
		if !ok {
			t.Errorf("%s: no --control-border token", name)
			continue
		}
		gbg, ga := glass(t, src, header)
		glassPage := over(gbg, m["surface-900"], ga)
		bgs := map[string]rgb{
			"surface-900":            m["surface-900"],
			"surface-800":            m["surface-800"],
			"surface-700":            m["surface-700"],
			"glass over surface-900": glassPage,
			"glass over surface-800": over(gbg, m["surface-800"], ga),
			// Non-surface backdrops that hold a bordered control (backdrop
			// scan, design §11.3 review 474e F1). Tailwind default colours.
			"ink-900 (fixit_doctor field row)":               m["ink-900"],
			"amber-500/5 glass panel over page (setup.html)": over(rgb{245, 158, 11}, glassPage, .05),
			"rose danger zone over page (memory_operator_detail)": map[string]rgb{
				"light": rgb{255, 241, 242},                         // bg-rose-50
				"dark":  over(rgb{76, 5, 25}, m["surface-900"], .2), // dark:bg-rose-950/20
			}[name],
		}
		for bn, bg := range bgs {
			got := contrast(cb, bg)
			if got < 3 {
				t.Errorf("%s: control-border vs %s = %.3f:1, want >= 3:1", name, bn, got)
				continue
			}
			t.Logf("%s: control-border vs %s = %.2f:1", name, bn, got)
		}
	}
	if !strings.Contains(src, "control: 'rgb(var(--control-border) / <alpha-value>)'") {
		t.Error("Tailwind config does not register the `control` colour, so border-control does not resolve")
	}
}

func TestPalette_NoThirdPartyFontsAndBrandChrome(t *testing.T) {
	src := partials(t)
	for _, bad := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "Bricolage", "Hanken", "peach:", "coral:", "sage:"} {
		if strings.Contains(src, bad) {
			t.Errorf("_partials.html still contains %q", bad)
		}
	}
	for _, want := range []string{"/ui/static/fonts/SpaceGrotesk-Variable.woff2", "/ui/static/fonts/Manrope-Variable.woff2", "#F6F4EF", "#10141A"} {
		if !strings.Contains(src, want) {
			t.Errorf("_partials.html lacks %q", want)
		}
	}
	for _, old := range []string{"#558A98", "#0078C3"} {
		if strings.Contains(src, "content=\""+old) || strings.Contains(src, "'"+old+"'") {
			t.Errorf("_partials.html theme-color still uses retired %s", old)
		}
	}
	for _, mf := range []string{"static/manifest.webmanifest", "static/approve.webmanifest"} {
		raw, err := os.ReadFile(mf)
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range []string{"#558A98", "#0078C3", "#3B4252", "#F5F1EC"} {
			if strings.Contains(strings.ToUpper(string(raw)), old) {
				t.Errorf("%s still uses retired %s", mf, old)
			}
		}
	}
}

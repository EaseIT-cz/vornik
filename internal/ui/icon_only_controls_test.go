// Icon-only links and buttons: accessible name (SC 4.1.2) and glyph
// non-text contrast (SC 1.4.11, 3:1). Design:
// https://docs.vornik.io §11.4 (c).
//
// audit 2026-10-12 (GitHub #335 item 3): icon-only <a>/<button> had never
// been surveyed. The fork-modal close button in replay.html had no
// accessible name and a gray-500 glyph at 2.92:1 on its light modal glass.
// This test scans both template trees, publishes the denominator, and
// requires every icon-only control to be in iconOnlyTable (so a new one is
// measured when it is added).
package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// One pattern per tag so each match closes on its own tag (Go regexp
	// has no backreferences). <summary> is a disclosure control too.
	ioCtls = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<(a)\b([^>]*)>(.*?)</a>`),
		regexp.MustCompile(`(?s)<(button)\b([^>]*)>(.*?)</button>`),
		regexp.MustCompile(`(?s)<(summary)\b([^>]*)>(.*?)</summary>`),
	}
	ioSVG      = regexp.MustCompile(`(?s)<svg\b.*?</svg>`)
	ioSROnly   = regexp.MustCompile(`(?s)<(span|div)\b[^>]*\bsr-only\b[^>]*>.*?</(?:span|div)>`)
	ioTplCall  = regexp.MustCompile(`\{\{-?\s*template\s+"[^"]*[Ii]con[^"]*"[^}]*\}\}`) // icon partials only; other templates render text
	ioTag      = regexp.MustCompile(`<[^>]+>`)
	ioAction   = regexp.MustCompile(`\{\{.*?\}\}`)
	ioStyle    = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	ioImgAlt   = regexp.MustCompile(`<img\b[^>]*\balt="[^"]+"`)
	ioNameAttr = regexp.MustCompile(`\b(aria-label|aria-labelledby|title)="[^"]+"`)
)

// backdrop names a surface an icon sits on; resolved per theme.
type backdrop string

const (
	bdPage       backdrop = "page (surface-900)"
	bdCard       backdrop = "card (surface-800)"
	bdRaised     backdrop = "raised (surface-700)"
	bdGlassPage  backdrop = "glass over page"
	bdModalGlass backdrop = "glass over black/60 over page"
	bdSheetGlass backdrop = "glass over black/50 over page"
	bdBrand500   backdrop = "brand-500"
)

func resolveBackdrop(t *testing.T, b backdrop, m map[string]rgb, gbg rgb, ga float64) rgb {
	t.Helper()
	black := rgb{0, 0, 0}
	switch b {
	case bdPage:
		return m["surface-900"]
	case bdCard:
		return m["surface-800"]
	case bdRaised:
		return m["surface-700"]
	case bdGlassPage:
		return over(gbg, m["surface-900"], ga)
	// Modal and sheet scrims are composited over the page. Over a white
	// card in light the scrim is (102,102,102) vs (98,98,96) over the page,
	// within rounding (review-20261012-057d F4).
	case bdModalGlass:
		return over(gbg, over(black, m["surface-900"], .6), ga)
	case bdSheetGlass:
		return over(gbg, over(black, m["surface-900"], .5), ga)
	case bdBrand500:
		return m["brand-500"]
	}
	t.Fatalf("unknown backdrop %q", b)
	return rgb{}
}

type glyphPair struct {
	// glyph is "" for the element's resting text colour (its unprefixed
	// text-gray-N / text-ink-N class, read from the markup), or a token name
	// or "white" for a glyph coloured by a CSS class (cssClass).
	glyph string
	on    backdrop
}

type iconOnly struct {
	file, marker string // marker: a substring of the element's attributes
	cssClass     string // for CSS-coloured glyphs: the class that colours them (checked present)
	pairs        []glyphPair
}

// iconOnlyTable is every icon-only control in the two template trees, with
// the backdrops it sits on (design §11.4 table).
var iconOnlyTable = []iconOnly{
	{"_partials.html", `aria-label="Vornik — Dashboard"`, "", []glyphPair{{"", bdGlassPage}}},
	{"_partials.html", `id="theme-toggle"`, "", []glyphPair{{"", bdGlassPage}}},
	// .rail-ico: ink-300 at rest; .rail-ico-active: white on brand-500.
	{"_partials.html", `data-rail="{{.Key}}"`, "rail-ico", []glyphPair{{"ink-300", bdGlassPage}, {"white", bdBrand500}}},
	{"_partials.html", `title="Sign out" class="rail-ico"`, "rail-ico", []glyphPair{{"ink-300", bdGlassPage}}},
	{"replay.html", `onclick="closeForkModal()"`, "", []glyphPair{{"", bdModalGlass}}},
	{"task_detail.html", `aria-label="Back"`, "", []glyphPair{{"", bdPage}}},
	{"task_detail.html", `aria-label="Previous task"`, "", []glyphPair{{"", bdPage}}},
	{"task_detail.html", `aria-label="Next task"`, "", []glyphPair{{"", bdPage}}},
	{"task_detail.html", `onclick="composeSheet.close()"`, "", []glyphPair{{"", bdSheetGlass}}},
	{"task_detail.html", `title="Download"`, "", []glyphPair{{"", bdRaised}}},
	// .fab: white on brand-500.
	{"tasks.html", `aria-label="Create new task"`, "fab", []glyphPair{{"white", bdBrand500}}},
}

// ioRestText is an UNPREFIXED text colour class: a variant (hover:, dark:)
// is preceded by ':' and does not match.
var ioRestText = regexp.MustCompile(`(?:^|\s)text-(?:gray|ink)-([1-9]00)(?:\s|$)`)

func hasClass(attrs, class string) bool {
	m := ctlClass.FindStringSubmatch(attrs)
	if m == nil {
		return false
	}
	// A class may abut a template action: `{{end}}rail-ico{{if ...}}`.
	return regexp.MustCompile(`(?:^|[\s}])` + regexp.QuoteMeta(class) + `(?:[\s{]|$)`).MatchString(m[1])
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestIconOnlyControls_NameAndGlyphContrast(t *testing.T) {
	var files []string
	for _, g := range []string{"templates/*.html", "../approverdevice/templates/*.html"} {
		fs, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fs...)
	}
	src := partials(t)
	themes := map[string]map[string]rgb{}
	glassBG := map[string]rgb{}
	glassA := map[string]float64{}
	for theme, header := range themeHeaders {
		themes[theme] = themeTokens(t, src, header)
		glassBG[theme], glassA[theme] = glass(t, src, header)
	}

	// A text-gray-N glyph is measured as ink-N. That is what it renders: the
	// console's Tailwind config maps the gray family onto the ink tokens
	// (review-20261012-057d F1 asked). Pin the mapping so the alias holds.
	// The config lives in internal/ui/tailwind since #165.
	cfg := tailwindConfigSource(t)
	for _, n := range []string{"100", "200", "300", "400", "500", "600"} {
		if !regexp.MustCompile(`(?s)gray:\s*\{[^}]*` + n + `: 'rgb\(var\(--ink-` + n + `\) / <alpha-value>\)'`).MatchString(cfg) {
			t.Fatalf("Tailwind gray-%s no longer maps to --ink-%s; the glyph measurements below assume it does", n, n)
		}
	}
	// The scrim alphas resolveBackdrop assumes (review-20261012-6d11): a
	// changed scrim must be re-measured, not silently mis-modelled.
	for file, want := range map[string]string{
		"templates/replay.html":      `id="fork-modal" class="hidden fixed inset-0 z-50 flex items-center justify-center bg-black/60"`,
		"templates/task_detail.html": `id="composeSheet" class="md:hidden bottom-0 left-0 right-0 m-0 mb-0 mt-auto w-full max-w-full p-0 glass border-t border-surface-600 backdrop:bg-black/50`,
	} {
		if !strings.Contains(string(readFile(t, file)), want) {
			t.Errorf("%s: scrim changed from the modelled one (%s); re-measure the close glyph", file, want)
		}
	}
	examined, found := 0, 0
	perTag := map[string]int{}
	matched := map[int]bool{}
	for _, f := range files {
		raw := ioStyle.ReplaceAllString(stripNonMarkup(string(readFile(t, f))), "")
		base := filepath.Base(f)
		var ctls [][]string
		for _, re := range ioCtls {
			ctls = append(ctls, re.FindAllStringSubmatch(raw, -1)...)
		}
		for _, m := range ctls {
			examined++
			perTag[m[1]]++
			attrs, body := m[2], m[3]
			text := ioSVG.ReplaceAllString(body, "")
			text = ioSROnly.ReplaceAllString(text, "")
			text = ioTplCall.ReplaceAllString(text, "")
			text = ioTag.ReplaceAllString(text, "")
			text = ioAction.ReplaceAllString(text, "X")
			text = strings.TrimSpace(strings.ReplaceAll(text, "&nbsp;", ""))
			if text != "" {
				continue
			}
			found++
			idx := -1
			for i, e := range iconOnlyTable {
				if e.file == base && strings.Contains(attrs, e.marker) {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Errorf("%s: icon-only <%s %s> is not in iconOnlyTable: measure its glyph contrast and add it (design §11.4)", base, m[1], strings.Join(strings.Fields(attrs), " "))
				continue
			}
			matched[idx] = true
			e := iconOnlyTable[idx]
			if !ioNameAttr.MatchString(attrs) && !ioSROnly.MatchString(body) && !strings.Contains(body, "<title>") && !ioImgAlt.MatchString(body) {
				t.Errorf("%s %s: icon-only control has no accessible name (aria-label, title or sr-only text)", base, e.marker)
			}
			if e.cssClass != "" && !hasClass(attrs, e.cssClass) {
				t.Errorf("%s %s: glyph class %q not on the element; re-measure and update the table", base, e.marker, e.cssClass)
			}
			restGlyph := ""
			if cm := ctlClass.FindStringSubmatch(attrs); cm != nil {
				if rm := ioRestText.FindStringSubmatch(cm[1]); rm != nil {
					restGlyph = "ink-" + rm[1]
				}
			}
			for theme, tm := range themes {
				for _, p := range e.pairs {
					glyph := p.glyph
					if glyph == "" {
						if restGlyph == "" {
							t.Errorf("%s %s: no resting text-gray-N/text-ink-N class to measure", base, e.marker)
							continue
						}
						glyph = restGlyph
					}
					fg := white
					if glyph != "white" {
						fg = tm[glyph]
					}
					bg := resolveBackdrop(t, p.on, tm, glassBG[theme], glassA[theme])
					got := contrast(fg, bg)
					if got < 3 {
						t.Errorf("%s: %s %s glyph %s on %s = %.2f:1, want >= 3:1", theme, base, e.marker, glyph, p.on, got)
						continue
					}
					t.Logf("%s: %s %s glyph %s on %s = %.2f:1", theme, base, e.marker, glyph, p.on, got)
				}
			}
		}
	}
	for i, e := range iconOnlyTable {
		if !matched[i] {
			t.Errorf("iconOnlyTable entry %s %s matched no icon-only control (stale entry)", e.file, e.marker)
		}
	}
	t.Logf("denominator: %d examined (a %d, button %d, summary %d) across %d templates; %d icon-only", examined, perTag["a"], perTag["button"], perTag["summary"], len(files), found)
}

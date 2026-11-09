package ui

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// GitHub #486 (operator decision A13; web-ui-design.md "The console service
// worker, the CSP plan and the placeholder colour", §D and §F): placeholder
// text meets WCAG 4.5:1 on the console's card and page surfaces in both
// themes. The colour is read from the stylesheet source and the theme tokens,
// so a palette change that breaks the ratio fails here.

var placeholderSurfaces486 = map[string][]string{
	// card, page: the surfaces measured in web-ui-design.md §D.
	"light": {"#FFFFFF", "#F6F4EF"},
	"dark":  {"#171C23", "#10141A"},
}

func TestPlaceholderContrast_486(t *testing.T) {
	css, err := os.ReadFile("tailwind/input.css")
	if err != nil {
		t.Fatal(err)
	}
	partials, err := os.ReadFile("templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	for theme, surfaces := range placeholderSurfaces486 {
		fg := placeholderColour486(t, string(css), string(partials), theme)
		for _, bg := range surfaces {
			if r := contrast486(fg, hexRGB486(t, bg)); r < 4.5 {
				t.Errorf("%s theme: placeholder %v on %s has contrast %.2f, want >= 4.5", theme, fg, bg, r)
			}
		}
	}
}

// placeholderColour486 resolves the placeholder colour a theme renders: the
// [data-theme="dark"] rule for dark (it wins by specificity), the plain rule
// for light. The paths are relative to the package directory, where go test
// runs. A colour is a
// hex literal or rgb(var(--token)), resolved from that theme's token block.
func placeholderColour486(t *testing.T, css, partials, theme string) [3]float64 {
	t.Helper()
	rule := regexp.MustCompile(`(?s)((?:\[data-theme="dark"\]\s+)?)input::placeholder,\s*(?:\[data-theme="dark"\]\s+)?textarea::placeholder\s*\{\s*color:\s*([^;]+);`)
	var val string
	for _, m := range rule.FindAllStringSubmatch(css, -1) {
		dark := strings.TrimSpace(m[1]) != ""
		if (theme == "dark") == dark {
			val = strings.TrimSpace(m[2])
		}
	}
	if val == "" {
		// Both rules are required: the light one alone fails 4.5:1 on dark
		// surfaces only by luck of the palette, so its absence is a defect.
		t.Fatalf("%s: no placeholder colour rule for this theme in input.css", theme)
	}
	if strings.HasPrefix(val, "#") {
		return hexRGB486(t, val)
	}
	tok := regexp.MustCompile(`rgb\(var\((--[a-z0-9-]+)\)`).FindStringSubmatch(val)
	if tok == nil {
		t.Fatalf("%s: unsupported placeholder colour %q", theme, val)
	}
	open := `:root, [data-theme="light"] {`
	if theme == "dark" {
		open = `[data-theme="dark"] {`
	}
	start := strings.Index(partials, open)
	if start < 0 {
		t.Fatalf("%s: token block %q not found in _partials.html", theme, open)
	}
	block := partials[start:]
	end := strings.Index(block, "}")
	if end < 0 {
		t.Fatalf("%s: token block %q is not closed", theme, open)
	}
	block = block[:end]
	m := regexp.MustCompile(regexp.QuoteMeta(tok[1]) + `:\s*(\d+)\s+(\d+)\s+(\d+);`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("%s: token %s not defined", theme, tok[1])
	}
	var c [3]float64
	for i := range c {
		v, _ := strconv.Atoi(m[i+1])
		c[i] = float64(v)
	}
	return c
}

func hexRGB486(t *testing.T, h string) [3]float64 {
	t.Helper()
	h = strings.TrimPrefix(h, "#")
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q", h)
		}
		c[i] = float64(v)
	}
	return c
}

func contrast486(a, b [3]float64) float64 {
	lum := func(c [3]float64) float64 {
		var l [3]float64
		for i, v := range c {
			s := v / 255
			if s <= 0.03928 {
				l[i] = s / 12.92
			} else {
				l[i] = math.Pow((s+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

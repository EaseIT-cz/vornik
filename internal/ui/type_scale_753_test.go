package ui

// Design system v2 part B (https://docs.vornik.io
// §12.2 B, #753): the console uses five type steps and nothing smaller than
// 13px. Before #753, 2,443 of 3,606 sized uses (68%) were 12px or smaller,
// including 764 arbitrary 9-11px classes.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// arbitraryTextSize matches a Tailwind arbitrary font size such as
// text-[11px] or text-[0.7rem]. Colour arbitraries (text-[#fff]) do not match.
var arbitraryTextSize = regexp.MustCompile(`text-\[[0-9.]+(px|rem|em)\]`)

// typeScaleEmitters are the files whose class literals reach the console
// stylesheet: the Tailwind content globs in internal/ui/tailwind/tailwind.config.js.
func typeScaleEmitters(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, glob := range []string{"templates/*.html", "static/console.js", "*.go", "../api/setup_handlers.go"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	return files
}

func TestTypeScale_NoArbitraryTextSizes_753(t *testing.T) {
	files := typeScaleEmitters(t)
	found := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range arbitraryTextSize.FindAllString(string(raw), -1) {
			found++
			if found <= 10 {
				t.Errorf("%s uses %s; use a named step (text-xs is the 13px caption)", f, m)
			}
		}
	}
	t.Logf("examined %d class-emitting files; %d arbitrary text sizes", len(files), found)
	if found > 10 {
		t.Errorf("... and %d more arbitrary text sizes", found-10)
	}
	// A glob that stops matching (a moved directory) must not turn this
	// scan vacuous: 246 files matched when #753 shipped.
	if len(files) < 200 {
		t.Errorf("examined only %d class-emitting files; the globs no longer match the console", len(files))
	}
}

// offScaleTextSize matches Tailwind's sizes above the scale (text-5xl to
// text-9xl, 48px and up), which the config leaves at Tailwind's defaults.
var offScaleTextSize = regexp.MustCompile(`\btext-[5-9]xl\b`)

// TestTypeScale_NoOffScaleSizes_753: none was used when #753 shipped; a new
// one would sit outside the five steps.
func TestTypeScale_NoOffScaleSizes_753(t *testing.T) {
	for _, f := range typeScaleEmitters(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := offScaleTextSize.FindString(string(raw)); m != "" {
			t.Errorf("%s uses %s; the largest step is title-l (text-3xl)", f, m)
		}
	}
}

// TestTypeScale_NamedStepsAreTheScale_753 pins the compiled stylesheet's
// named sizes to the five steps (caption 13/18, body 15/22, title-s 18/26,
// title-m 24/32, title-l 32/40).
func TestTypeScale_NamedStepsAreTheScale_753(t *testing.T) {
	raw, err := os.ReadFile("static/tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	want := map[string]string{
		"text-xs":   "font-size:13px;line-height:18px",
		"text-sm":   "font-size:15px;line-height:22px",
		"text-base": "font-size:15px;line-height:22px",
		"text-lg":   "font-size:18px;line-height:26px",
		"text-xl":   "font-size:18px;line-height:26px",
		"text-2xl":  "font-size:24px;line-height:32px",
		"text-3xl":  "font-size:32px;line-height:40px",
		"text-4xl":  "font-size:32px;line-height:40px",
	}
	// Tailwind merges equal rules into one selector list
	// (.text-3xl,.text-4xl{…}), so match the class inside a list.
	for class, decl := range want {
		re := regexp.MustCompile(`(^|[,}])[^{}]*\.` + regexp.QuoteMeta(class) + `(,[^{}]*)?\{` + regexp.QuoteMeta(decl) + `\}`)
		if !re.MatchString(css) {
			t.Errorf("static/tailwind.css lacks .%s{%s} (run make ui-css after the config change)", class, decl)
		}
	}
}

// TestTypeScale_HandWrittenCSSFloor_753 refuses a hand-written font-size
// below 13px in any console template or console.js. An em size is refused
// outright: its pixel size depends on the parent, so the floor cannot be
// checked. The workflow-graph node labels drawn by script in
// project_detail.html move with the graph palette (§12.2 part E, #757) and
// are exempt until then.
func TestTypeScale_HandWrittenCSSFloor_753(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "static/console.js")
	decl := regexp.MustCompile(`font-size:\s*([0-9.]+)(px|rem|em)\b`)
	examined := 0
	for _, f := range files {
		if f == filepath.Join("templates", "project_detail.html") {
			continue // part E, #757
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(raw), -1) {
			examined++
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			switch m[2] {
			case "em":
				t.Errorf("%s sets %s; use px or rem so the 13px floor can be checked", f, m[0])
			case "rem":
				v *= 16
			}
			if m[2] != "em" && v < 13 {
				t.Errorf("%s sets %s (%.1fpx); the floor is 13px", f, m[0], v)
			}
		}
	}
	t.Logf("examined %d font-size declarations in %d files", examined, len(files)-1)
}

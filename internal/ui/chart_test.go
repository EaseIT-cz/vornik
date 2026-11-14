package ui

// The shared bar chart (#757; design §12.2 E). Before it the Insights
// charts drew bars only (no axis, no scale, no dates) in hard-coded
// Tailwind hex colours, and the dark-theme SVG text (#e5e7eb) nearly
// vanished on the light default.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestNewBarChart_Geometry_757(t *testing.T) {
	c := newBarChart([]float64{2, 8, 4}, []string{"2026-11-01", "2026-11-02", "2026-11-03"}, []string{"a", "b", "c"}, countUnit("task", "tasks"))
	if len(c.Bars) != 3 {
		t.Fatalf("bars = %d, want 3", len(c.Bars))
	}
	if c.Bars[1].H != chartPlotH || c.Bars[1].Y != c.Top {
		t.Errorf("the highest bar should fill the plot: %+v", c.Bars[1])
	}
	if c.Bars[0].H != chartPlotH/4 || c.Bars[0].Y+c.Bars[0].H != c.Baseline {
		t.Errorf("bars stand on the baseline at scale: %+v", c.Bars[0])
	}
	if !c.Bars[2].Current || c.Bars[0].Current || c.Bars[1].Current {
		t.Error("only the newest bar is current")
	}
	if c.YMax != "8" || c.YMid != "4" {
		t.Errorf("scale labels = %q, %q", c.YMax, c.YMid)
	}
	var texts []string
	for _, l := range c.XLabels {
		texts = append(texts, l.Text)
	}
	if strings.Join(texts, ",") != "1 Nov,2 Nov,3 Nov" {
		t.Errorf("date labels = %v", texts)
	}
	if c.Answer != "Highest: 8 tasks on 2 Nov. Today: 4 tasks." {
		t.Errorf("answer = %q", c.Answer)
	}
	if c.Width != chartLeft+3*chartColW+chartRight {
		t.Errorf("width = %d", c.Width)
	}
}

func TestNewBarChart_Empty_757(t *testing.T) {
	c := newBarChart([]float64{0, 0}, []string{"2026-11-01", "2026-11-02"}, nil, dollarUnit)
	if c.Answer != "Nothing recorded in the last 2 days." {
		t.Errorf("answer = %q", c.Answer)
	}
	for _, b := range c.Bars {
		if b.H != 0 {
			t.Errorf("an empty day has no bar: %+v", b)
		}
	}
}

// The partial draws from tokens only, labels its scale and dates, and
// carries the answer as its text alternative.
func TestBarChartPartial_757(t *testing.T) {
	c := newBarChart([]float64{1, 3}, []string{"2026-11-12", "2026-11-13"}, []string{"x", "y"}, countUnit("task", "tasks"))
	html := renderTemplateString(t, "barChart", c)
	for _, want := range []string{`role="img"`, `aria-label="Highest: 3 tasks on 13 Nov. Today: 3 tasks."`, ">3<", ">12 Nov<", ">13 Nov<", `class="c-bar-now"`, `class="c-bar"`, `class="c-grid"`, `class="c-axis"`, "<title>y</title>"} {
		if !strings.Contains(html, want) {
			t.Errorf("barChart lacks %s:\n%s", want, html)
		}
	}
	if m := regexp.MustCompile(`#[0-9a-fA-F]{6}`).FindString(html); m != "" {
		t.Errorf("barChart uses the literal colour %s", m)
	}
}

// chartTemplates hold charts or graphs; none may carry a literal colour
// (design §12.2 E: the chart palette is tokens, in both themes).
var chartTemplates = []string{
	"workflow_graph", "composer_preview", "trading_dashboard",
	"memory_subgraph", "insights_trends", "insights_tool_budget", "project_config",
	// #759 M3: the last two, once pending.
	"memory_project", "project_detail",
}

// Page script that builds colours from tokens must not append a hex alpha
// to one ('…'+colors.border+'30' made rgb(var(--x))30, invalid CSS; code
// review review-20261114-c13f F1), and must not write a token into an SVG
// presentation attribute through setAttribute.
func TestWorkflowGraph_TokenColoursStayValid_759(t *testing.T) {
	raw, err := os.ReadFile("templates/project_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if regexp.MustCompile(`colors\.\w+\s*\+\s*'[0-9a-fA-F]{2}`).MatchString(s) {
		t.Error("project_detail.html appends a hex alpha to a token colour")
	}
	for _, bad := range []string{`setAttribute('fill', strokeColor`, `setAttribute('stroke', strokeColor`, `setAttribute('fill','rgb(`, `setAttribute('stroke','rgb(`} {
		if strings.Contains(s, bad) {
			t.Errorf("project_detail.html writes a token colour into an SVG attribute: %s", bad)
		}
	}
}

// The scatter's fills and rings come from Go as classes (#759 M3): no
// rendered colour may sit in an SVG presentation attribute, where a var()
// is dropped.
func TestMemoryScatter_NoColourAttributes_759(t *testing.T) {
	raw, err := os.ReadFile("templates/memory_project.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`fill="{{classColour`, `stroke="{{statusStrokeColour`, `fill="rgb(`, `stroke="rgb(`, `style="background-color: {{`} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("memory_project.html still sets a colour through %s", bad)
		}
	}
	if !strings.Contains(string(raw), `class="{{classColour $p.ContentClass}} {{statusStrokeColour $p.Status}}"`) {
		t.Error("the scatter point should take its fill and ring as classes")
	}
}

func TestChartTemplates_NoLiteralColour_757(t *testing.T) {
	hex := regexp.MustCompile(`#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{3}\b`)
	issue := regexp.MustCompile(`#[0-9]{3}\b`) // "#748" in a comment is an issue, not a colour
	for _, name := range chartTemplates {
		raw, err := os.ReadFile("templates/" + name + ".html")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range hex.FindAllString(string(raw), -1) {
			if issue.MatchString(m) {
				continue
			}
			t.Errorf("%s.html uses the literal colour %s; use a chart or status token", name, m)
		}
	}
}

// Chart marks are graphical objects: 3:1 against the card and the page in
// both themes (WCAG 1.4.11; design §12.2 E values table). The binding pair
// is chart-muted on the light card, 3.16:1.
func TestChartTokens_Contrast_757(t *testing.T) {
	src := partials(t)
	for theme, header := range themeHeaders {
		m := themeTokens(t, src, header)
		for _, tk := range []string{"chart-1", "chart-2", "chart-3", "chart-4", "chart-5", "chart-muted", "chart-threshold"} {
			c, ok := m[tk]
			if !ok {
				t.Fatalf("%s: --%s missing", theme, tk)
			}
			for _, g := range []string{"surface-800", "surface-900"} {
				if got := contrast(c, m[g]); got < 3 {
					t.Errorf("%s: --%s on %s is %.2f:1, want 3:1", theme, tk, g, got)
				}
			}
		}
		// Chart labels are text: ink-400 at 4.5:1.
		for _, g := range []string{"surface-800", "surface-900"} {
			if got := contrast(m["ink-400"], m[g]); got < 4.5 {
				t.Errorf("%s: chart label ink-400 on %s is %.2f:1, want 4.5:1", theme, g, got)
			}
		}
	}
}

// Label placement for 1, 2 and 14 bars (review-20261113-d970 F3).
func TestNewBarChart_LabelPlacement_757(t *testing.T) {
	dates := func(n int) []string {
		var d []string
		for i := 1; i <= n; i++ {
			d = append(d, fmt.Sprintf("2026-11-%02d", i))
		}
		return d
	}
	vals := func(n int) []float64 {
		v := make([]float64, n)
		for i := range v {
			v[i] = float64(i + 1)
		}
		return v
	}
	for _, tc := range []struct {
		n       int
		anchors string
		texts   string
	}{
		{1, "middle", "1 Nov"},
		{2, "start,end", "1 Nov,2 Nov"},
		{14, "start,middle,end", "1 Nov,8 Nov,14 Nov"},
	} {
		c := newBarChart(vals(tc.n), dates(tc.n), nil, countUnit("task", "tasks"))
		var anchors, texts []string
		for _, l := range c.XLabels {
			anchors = append(anchors, l.Anchor)
			texts = append(texts, l.Text)
			if l.X < c.Left || l.X > c.PlotRight {
				t.Errorf("n=%d: label %q at x=%d is outside the plot [%d, %d]", tc.n, l.Text, l.X, c.Left, c.PlotRight)
			}
		}
		if strings.Join(anchors, ",") != tc.anchors || strings.Join(texts, ",") != tc.texts {
			t.Errorf("n=%d: labels %v anchored %v, want %s / %s", tc.n, texts, anchors, tc.texts, tc.anchors)
		}
		if c.PlotRight != chartLeft+tc.n*chartColW {
			t.Errorf("n=%d: PlotRight %d", tc.n, c.PlotRight)
		}
	}
}

// The scale labels match their gridlines and fit the 48px gutter
// (review-20261113-d970 F1, F4): an odd max shows the exact half, a large
// count is compact, and no unit word rides on the scale.
func TestNewBarChart_ScaleLabels_757(t *testing.T) {
	c := newBarChart([]float64{7, 2}, []string{"2026-11-12", "2026-11-13"}, nil, countUnit("task", "tasks"))
	if c.YMax != "7" || c.YMid != "3.5" {
		t.Errorf("odd max: scale %q / %q, want 7 / 3.5", c.YMax, c.YMid)
	}
	c = newBarChart([]float64{1234, 10}, []string{"2026-11-12", "2026-11-13"}, nil, countUnit("task", "tasks"))
	if c.YMax != "1.2k" || c.YMid != "617" {
		t.Errorf("large max: scale %q / %q, want 1.2k / 617", c.YMax, c.YMid)
	}
	c = newBarChart([]float64{2.5, 1}, []string{"2026-11-12", "2026-11-13"}, nil, dollarUnit)
	if c.YMax != "$2.50" || c.YMid != "$1.25" {
		t.Errorf("dollars: scale %q / %q", c.YMax, c.YMid)
	}
	// 13px at up to 8px a character, inside the gutter left of YLabelX.
	for _, l := range []string{c.YMax, "1.2k", scaleLabel(999.99, dollarUnit), scaleLabel(99.99, dollarUnit), scaleLabel(1_234_567, dollarUnit)} {
		if w := 8 * len(l); w > c.YLabelX {
			t.Errorf("scale label %q is %dpx, wider than the %dpx gutter", l, w, c.YLabelX)
		}
	}
	if newBarChart(nil, nil, nil, dollarUnit).Answer != "Nothing recorded yet." {
		t.Error("an empty chart should say so in plain words")
	}
}

// The visible answer and the text alternative are the same sentence.
func TestBarChartPartial_AnswerIsTheTextAlternative_757(t *testing.T) {
	c := newBarChart([]float64{1, 3}, []string{"2026-11-12", "2026-11-13"}, []string{"x"}, countUnit("task", "tasks"))
	html := renderTemplateString(t, "barChart", c)
	if !strings.Contains(html, `aria-label="`+c.Answer+`"`) || !strings.Contains(html, ">"+c.Answer+"</p>") {
		t.Errorf("answer %q is not both the aria-label and the visible sentence:\n%s", c.Answer, html)
	}
	// A bar with no tooltip renders an empty title, not a panic.
	if !strings.Contains(html, "<title></title>") {
		t.Error("the second bar (no tooltip) should render an empty title")
	}
}

// The trading chart's gutters hold its longest axis labels at 13px
// (review-20261113-d970 F7): eight characters at up to 8px each, inside the
// 6px inset.
func TestPerfChart_GutterHoldsLongestLabel_757(t *testing.T) {
	left := "+" + fmtAxisUSD(999_949) // "+$999.9k"
	hi, _ := fmtEquityAxisPair(1_052_629, 1_052_628)
	for _, c := range []struct {
		label  string
		gutter int
	}{{left, perfChartLeftGutter}, {hi, perfChartRightGutter}} {
		if w := 8*len(c.label) + 6; w > c.gutter {
			t.Errorf("label %q needs %dpx, gutter is %dpx", c.label, w, c.gutter)
		}
	}
}

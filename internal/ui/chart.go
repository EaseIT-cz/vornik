package ui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// barChart is the one bar-chart shape the console draws (#757; design
// ui-design-refresh-design.md §12.2 E and ui-modernisation-spike.md D4).
// Before this chart (#757), each Insights chart repeated the same geometry
// and drew bars only: no axis, no scale, no dates, nothing a reader could
// answer from without hovering. The template partial "barChart" in _partials.html
// renders it with labelled gridlines, the first, middle and last date, the
// current bar in the accent colour and an answer sentence that is also the
// chart's text alternative.
type barChart struct {
	Width, Height int
	Left          int // x where the plot starts; y labels sit left of it
	PlotRight     int // x where the last column ends; gridlines stop here
	Top, MidY     int // y of the max and half gridlines
	Baseline      int // y of the zero axis
	YLabelX       int
	YMax, YMid    string
	Bars          []chartBar
	XLabels       []chartXLabel
	XLabelY       int
	Answer        string
}

type chartBar struct {
	X, Y, W, H int
	Tooltip    string
	Current    bool // the newest bar (today), drawn in the accent colour
}

type chartXLabel struct {
	X      int
	Text   string
	Anchor string // SVG text-anchor
}

// Bar chart geometry in SVG user units. Labels are 13px (the caption step).
const (
	chartLeft   = 56
	chartTop    = 14
	chartPlotH  = 140
	chartColW   = 34
	chartBarW   = 22
	chartRight  = 8
	chartBottom = 30
)

// newBarChart lays out one bar per value. dates are YYYY-MM-DD (shown as
// "3 Nov") for a window that ends today, so the last bar is today.
// tooltips name each bar's detail; unit formats a value for the answer
// ("12 tasks", "$1.84"); the scale uses a compact number (scaleLabel) so a
// label always fits the 48px gutter. Pure, for tests.
func newBarChart(values []float64, dates, tooltips []string, unit func(float64) string) barChart {
	n := len(values)
	c := barChart{
		Left:      chartLeft,
		Top:       chartTop,
		MidY:      chartTop + chartPlotH/2,
		Baseline:  chartTop + chartPlotH,
		YLabelX:   chartLeft - 8,
		Width:     chartLeft + n*chartColW + chartRight,
		PlotRight: chartLeft + n*chartColW,
		Height:    chartTop + chartPlotH + chartBottom,
		XLabelY:   chartTop + chartPlotH + 20,
	}
	maxV, maxI := 0.0, -1
	for i, v := range values {
		if v > maxV {
			maxV, maxI = v, i
		}
	}
	c.YMax, c.YMid = scaleLabel(maxV, unit), scaleLabel(maxV/2, unit)
	for i, v := range values {
		h := 0
		if maxV > 0 {
			h = int(v * chartPlotH / maxV)
		}
		b := chartBar{
			X: chartLeft + i*chartColW + (chartColW-chartBarW)/2, W: chartBarW,
			H: h, Y: c.Baseline - h, Current: i == n-1,
		}
		if i < len(tooltips) {
			b.Tooltip = tooltips[i]
		}
		c.Bars = append(c.Bars, b)
	}
	if n > 0 {
		for _, i := range uniqueInts(0, n/2, n-1) {
			anchor := "middle"
			switch {
			case i == 0 && n > 1:
				anchor = "start"
			case i == n-1 && n > 1:
				anchor = "end"
			}
			x := chartLeft + i*chartColW + chartColW/2
			switch anchor {
			case "start":
				x = chartLeft + i*chartColW + (chartColW-chartBarW)/2
			case "end":
				x = chartLeft + i*chartColW + (chartColW+chartBarW)/2
			}
			c.XLabels = append(c.XLabels, chartXLabel{X: x, Text: shortDate(dateAt(dates, i)), Anchor: anchor})
		}
	}
	switch {
	case n == 0:
		c.Answer = "Nothing recorded yet."
	case maxV == 0:
		c.Answer = fmt.Sprintf("Nothing recorded in the last %d days.", n)
	default:
		c.Answer = fmt.Sprintf("Highest: %s on %s. Today: %s.", unit(maxV), shortDate(dateAt(dates, maxI)), unit(values[n-1]))
	}
	return c
}

// scaleLabel formats a scale value compactly, without the unit word so it
// fits the gutter: "8", "3.5", "1.2k" for counts; dollars keep "$".
// A half of an odd count shows its decimal, so the label matches its
// gridline exactly.
func scaleLabel(v float64, unit func(float64) string) string {
	prefix := ""
	if strings.HasPrefix(unit(1), "$") {
		prefix = "$"
		switch {
		case v < 100:
			return fmt.Sprintf("$%.2f", v)
		case v < 1000:
			return fmt.Sprintf("$%.0f", v)
		}
	}
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%s%.1fM", prefix, v/1_000_000)
	case v >= 1000:
		return fmt.Sprintf("%s%.1fk", prefix, v/1000)
	case v == math.Trunc(v):
		return fmt.Sprintf("%s%.0f", prefix, v)
	default:
		return fmt.Sprintf("%s%.1f", prefix, v)
	}
}

func dateAt(dates []string, i int) string {
	if i >= 0 && i < len(dates) {
		return dates[i]
	}
	return ""
}

// shortDate turns 2026-11-03 into "3 Nov"; anything else passes through.
func shortDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("2 Jan")
}

func uniqueInts(xs ...int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// countUnit formats a count for a scale or an answer: "12 tasks", "1 task".
func countUnit(singular, plural string) func(float64) string {
	return func(v float64) string {
		n := int(v + 0.5)
		if n == 1 {
			return "1 " + singular
		}
		return fmt.Sprintf("%d %s", n, plural)
	}
}

// dollarUnit formats spend: "$1.84".
func dollarUnit(v float64) string { return fmt.Sprintf("$%.2f", v) }

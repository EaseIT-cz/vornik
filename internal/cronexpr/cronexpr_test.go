package cronexpr

import (
	"testing"
	"time"
)

// Agent-administered Vornik design §17.3: a broker schedule's cron fields are
// evaluated in the workflow's zone, and the result is an instant. Control:
// NextFireAtIn.
func TestNextFireAtIn(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skip("no zoneinfo")
	}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := NextFireAtIn("0 8 * * *", after, prague)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC); !got.Equal(want) { // 08:00 CET
		t.Fatalf("got %s, want %s", got, want)
	}
	if _, err := NextFireAtIn("not cron", after, prague); err == nil {
		t.Fatal("a bad expression was accepted")
	}
	// Fall-back day: robfig returns the repeated 02:30 twice, as two
	// instants an hour apart (design §17.3; probed 2026-10-02). The
	// scheduler's key, not this helper, collapses them.
	first, _ := NextFireAtIn("30 2 * * *", time.Date(2026, 10, 24, 12, 0, 0, 0, prague), prague)
	second, _ := NextFireAtIn("30 2 * * *", first, prague)
	if second.Sub(first) != time.Hour || first.In(prague).Format("15:04") != second.In(prague).Format("15:04") {
		t.Fatalf("fall-back: %s then %s", first, second)
	}
}

// Design §17.1: two consecutive firings at least an hour apart, checked
// over the 50 firings after 2026-01-01T00:00Z in the zone. Control: MinGap.
func TestMinGap(t *testing.T) {
	for expr, want := range map[string]time.Duration{
		"0 * * * *":    time.Hour,
		"*/30 * * * *": 30 * time.Minute,
		"0 8 * * *":    24 * time.Hour,
		"0,30 8 * * *": 30 * time.Minute,
		"0 0 29 2 *":   0, // yearly-ish: just check it is long
	} {
		got, err := MinGap(expr, time.UTC, 50)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if want == 0 {
			if got < 365*24*time.Hour {
				t.Errorf("%s: %s", expr, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %s, want %s", expr, got, want)
		}
	}
}

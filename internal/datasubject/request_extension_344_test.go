package datasubject

import (
	"strings"
	"testing"
	"time"
)

// GitHub #590 (GDPR rights design §4.20.6). Found at: review, observed at
// integration (the deadline every surface prints): Art 12(3)'s "one month"
// was computed as 30 days, so for a request received on 31 January every
// warning ran two days late. Regulation 1182/71 Art 3(2)(c): the period ends
// with the last hour of the day in the last month with the same date, or of
// that month's last day.
func endOf(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).Add(-time.Nanosecond)
}

func TestDeadline_CalendarMonths(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 30, 0, 0, time.UTC) }
	cases := []struct {
		name            string
		opened          time.Time
		month, extended time.Time
	}{
		{"15 January", day(2026, time.January, 15), endOf(2026, time.February, 15), endOf(2026, time.April, 15)},
		{"31 January", day(2026, time.January, 31), endOf(2026, time.February, 28), endOf(2026, time.April, 28)},
		{"31 January, leap year", day(2028, time.January, 31), endOf(2028, time.February, 29), endOf(2028, time.April, 29)},
		{"30 November", day(2026, time.November, 30), endOf(2026, time.December, 30), endOf(2027, time.February, 28)},
		{"31 December", day(2026, time.December, 31), endOf(2027, time.January, 31), endOf(2027, time.March, 31)},
		// Received at 23:30 UTC: the UTC date is the day the period runs from.
		{"late UTC evening", time.Date(2026, time.March, 31, 23, 30, 0, 0, time.UTC), endOf(2026, time.April, 30), endOf(2026, time.June, 30)},
	}
	for _, c := range cases {
		r := openRequest(RequestErasure, c.opened)
		if got := r.Deadline(); !got.Equal(c.month) {
			t.Errorf("%s: Deadline = %s, want %s", c.name, got, c.month)
		}
		if got := FirstMonthEnd(c.opened); !got.Equal(c.month) {
			t.Errorf("%s: FirstMonthEnd = %s, want %s", c.name, got, c.month)
		}
		r.Extended = true
		if got := r.Deadline(); !got.Equal(c.extended) {
			t.Errorf("%s: extended Deadline = %s, want %s", c.name, got, c.extended)
		}
		if !r.NeedsAttention(c.extended.Add(-WarnBefore).Add(time.Second)) ||
			r.NeedsAttention(c.extended.Add(-WarnBefore).Add(-time.Second)) {
			t.Errorf("%s: the warning is not %s before the extended deadline", c.name, WarnBefore)
		}
	}
}

func TestRequest_ExtensionNeedsATimelyNotice(t *testing.T) {
	opened := time.Date(2026, time.January, 31, 9, 0, 0, 0, time.UTC)
	now := opened.Add(10 * 24 * time.Hour)
	limit := FirstMonthEnd(opened)
	bad := map[string]time.Time{
		"missing":       {},
		"in the future": now.Add(time.Hour),
		"before opened": opened.Add(-time.Hour),
	}
	for name, at := range bad {
		r := openRequest(RequestErasure, opened)
		if err := r.Extend("complex", at, now); err == nil {
			t.Errorf("%s notice date accepted", name)
		}
	}
	// After the first month: refused even if "now" is still early enough.
	late := openRequest(RequestErasure, opened)
	if err := late.Extend("complex", limit.Add(time.Second), limit.Add(2*time.Second)); err == nil ||
		!strings.Contains(err.Error(), "within one month") {
		t.Errorf("a notice after the first month = %v", err)
	}
	// A notice on day 31 of a 31-day month is in time (the 30-day code
	// refused it).
	jan15 := openRequest(RequestErasure, time.Date(2026, time.January, 15, 9, 0, 0, 0, time.UTC))
	day31 := time.Date(2026, time.February, 15, 12, 0, 0, 0, time.UTC)
	if err := jan15.Extend("complex", day31, day31); err != nil {
		t.Errorf("a notice on day 31 of the month = %v", err)
	}
	// Exactly at the limit is in time.
	r := openRequest(RequestErasure, opened)
	if err := r.Extend("complex", limit, limit); err != nil {
		t.Fatalf("notice at the limit: %v", err)
	}
	if !r.Extended || !r.ExtensionNotifiedAt.Equal(limit) {
		t.Fatalf("request = %+v", r)
	}
}

func TestEscalationMarginIsInsideTheWarning(t *testing.T) {
	if EscalationMargin >= WarnBefore || EscalationMargin != 7*24*time.Hour {
		t.Fatalf("EscalationMargin %s, WarnBefore %s", EscalationMargin, WarnBefore)
	}
}

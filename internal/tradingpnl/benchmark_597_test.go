package tradingpnl

import (
	"math"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #597: the book's daily change on strategy capital beside SPY close
// to close, on the sessions both have, with the running difference.
func TestVsBenchmark_597(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	equity := []DailyPoint{
		{Day: d(28), EquityUSD: 1_000_000},
		{Day: d(29), EquityUSD: 1_000_150}, // +150 on 15000 = +1%
		{Day: d(30), EquityUSD: 1_000_000}, // -150 = -1%
	}
	spy := []persistence.BenchmarkClose{
		{Session: d(28), Close: 500},
		{Session: d(29), Close: 505}, // +1%
		{Session: d(30), Close: 510.05},
		{Session: d(31), Close: 520}, // no book point: skipped
	}
	rows := VsBenchmark(equity, spy, 15000)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !rows[0].Day.Equal(d(29)) || !near(rows[0].BookPct, 1) || !near(rows[0].BenchPct, 1) || !near(rows[0].CumDiffPct, 0) {
		t.Fatalf("29th %+v", rows[0])
	}
	if !near(rows[1].BookPct, -1) || !near(rows[1].BenchPct, 1) || !near(rows[1].CumDiffPct, -2) {
		t.Fatalf("30th %+v", rows[1])
	}
	if VsBenchmark(equity, spy, 0) != nil {
		t.Fatal("no strategy capital: no line")
	}
}

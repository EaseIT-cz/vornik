package trading

import (
	"math"
	"testing"
	"time"
)

// GitHub #719: the outcome baseline is the series' close of the session the
// tick's scorecard last_close describes, the last one completed at the tick,
// and the series' own ATR14 through that session.
func TestBaseline_Close_719(t *testing.T) {
	tz, _ := time.LoadLocation("America/New_York")
	at := func(d, h int) time.Time { return time.Date(2026, time.November, d, h, 0, 0, 0, tz) }
	day := func(d int) time.Time { return time.Date(2026, time.November, d, 0, 0, 0, 0, time.UTC) }
	bars := []DailyBar{{Session: day(10), Close: 105}, {Session: day(6), Close: 99}, {Session: day(9), Close: 101}}
	gap := []DailyBar{{Session: day(6), Close: 99}, {Session: day(10), Close: 105}}
	thanksgiving := []DailyBar{{Session: day(25), Close: 110}, {Session: day(27), Close: 111}}

	cases := []struct {
		name   string
		bars   []DailyBar
		at     time.Time
		want   float64
		wantOK bool
	}{
		{"intraday Tue: Monday's close, not Tuesday's", bars, at(10, 11), 101, true},
		{"after the Tue close: Tuesday's own close", bars, at(10, 17), 105, true},
		{"intraday Mon: the weekend has no session, so Friday", bars, at(9, 11), 99, true},
		{"no completed session in the series", bars, at(6, 11), 0, false},
		{"Monday missing from the series: no baseline, never Friday's close", gap, at(10, 11), 0, false},
		{"Fri after Thanksgiving: Wednesday's close", thanksgiving, at(27, 11), 110, true},
	}
	for _, c := range cases {
		got, atr, ok := Baseline(c.bars, c.at)
		if got != c.want || ok != c.wantOK || atr != 0 {
			t.Errorf("%s: got %v %v (atr %v), want %v %v (atr 0: under 15 bars)", c.name, got, ok, atr, c.want, c.wantOK)
		}
	}
}

// The ATR is the scorecard's Wilder ATR14 (services/broker-ta indicators.atr):
// the mean of true ranges 1..14 seeds it, then alpha 1/14. Fourteen ranges of
// 2 seed 2; a 15th range of 16 gives (2*13+16)/14 = 3. Bars after the
// baseline session never count.
func TestBaseline_WilderATR14_719(t *testing.T) {
	start := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	var bars []DailyBar
	for i := 0; i < 15; i++ {
		bars = append(bars, DailyBar{Session: start.AddDate(0, 0, i), High: 102, Low: 100, Close: 101})
	}
	bars = append(bars, DailyBar{Session: start.AddDate(0, 0, 15), High: 110, Low: 94, Close: 101})
	bars = append(bars, DailyBar{Session: start.AddDate(0, 0, 16), High: 500, Low: 1, Close: 101})
	observed := time.Date(2026, time.October, 17, 15, 0, 0, 0, time.UTC) // 11:00 ET on the 17th
	_, atr, ok := Baseline(bars, observed)
	if !ok || math.Abs(atr-3) > 1e-9 {
		t.Fatalf("atr %v ok %v, want 3 true", atr, ok)
	}

	bars[3].High = 0 // a bar without a high: ATR unknown, close still known
	if c, atr, ok := Baseline(bars, observed); !ok || c != 101 || atr != 0 {
		t.Fatalf("got %v %v %v, want 101 0 true", c, atr, ok)
	}
}

package trading

import (
	"math"
	"testing"
	"time"
)

// GitHub #596: the entry signal is scored on its own, from a funnel the
// daemon records and bars it already fetches. Measurement only.

func TestParseFunnel_596(t *testing.T) {
	res := []byte(`{"proposals":[],"has_proposals":false,"entry_funnel":[
	 {"symbol":"AAPL","intent":"open","status":"rejected","gate":"regime","reason":"RISK_OFF","inputs":{"last_close":348.91,"atr14":6.2673}},
	 {"symbol":"","intent":"open","status":"rejected","gate":"x"}]}`)
	rows, ok := ParseFunnel(res)
	if !ok || len(rows) != 1 || rows[0].Symbol != "AAPL" {
		t.Fatalf("rows %+v ok %v", rows, ok)
	}
	c, a := RefPrices(rows[0].Inputs)
	if c != 348.91 || a != 6.2673 {
		t.Fatalf("ref prices %v %v", c, a)
	}
	for _, bad := range []string{`not json`, `{"proposals":[]}`} {
		if _, ok := ParseFunnel([]byte(bad)); ok {
			t.Errorf("%s: no funnel must report ok=false", bad)
		}
	}
}

// Every spelling measured on 2026-10-31 maps to its gate; anything else is
// "other".
func TestNormalizeGate_596(t *testing.T) {
	want := map[string]string{
		"regime": "regime", "spy_regime": "spy_regime",
		"scorecard_total": "scorecard", "scorecard_floor": "scorecard",
		"daily_trend": "daily_trend", "trend": "daily_trend", "daily_trend_alignment": "daily_trend",
		"held": "held", "held_already": "held", "already_held": "held", "held_add_eligibility": "held",
		"rsi_add_band": "rsi_band", "add_rsi_band": "rsi_band", "rsi_floor": "rsi_band", "rsi": "rsi_band",
		"earnings_proximity": "earnings", "rank": "rank", "none": "none", "data_quality": "data_quality",
		"made_up_gate": "other", "": "other",
	}
	if NormalizeGate("  Regime ") != "regime" {
		t.Error("case and surrounding space are ignored")
	}
	for raw, g := range want {
		if got := NormalizeGate(raw); got != g {
			t.Errorf("%q: got %q, want %q", raw, got, g)
		}
	}
}

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

// Thu 2026-10-29 observed; Fri 30, Mon 2, Tue 3, Wed 4, Thu 5 follow (the
// weekend has no bar, so it is no session).
var fwdBars = []DailyBar{
	{day(10, 29), 101, 99, 100},
	{day(10, 30), 104, 98, 103},
	{day(11, 2), 106, 101, 105},
	{day(11, 3), 102, 95, 96},
	{day(11, 4), 99, 94, 97},
	{day(11, 5), 110, 96, 108},
}

func TestForwardOutcome_596(t *testing.T) {
	obs, done := day(10, 29), day(11, 5)
	o1, ok := ForwardOutcome(fwdBars, obs, 100, 2, 1, done)
	if !ok || math.Abs(o1.Return-0.03) > 1e-9 || o1.MFE != 2 || o1.MAE != -1 {
		t.Fatalf("1 session: %+v %v", o1, ok)
	}
	o3, ok := ForwardOutcome(fwdBars, obs, 100, 2, 3, done)
	if !ok || math.Abs(o3.Return+0.04) > 1e-9 || o3.MFE != 3 || o3.MAE != -2.5 {
		t.Fatalf("3 sessions (across the weekend): %+v %v", o3, ok)
	}
	o5, ok := ForwardOutcome(fwdBars, obs, 100, 2, 5, done)
	if !ok || math.Abs(o5.Return-0.08) > 1e-9 || o5.MFE != 5 || o5.MAE != -3 {
		t.Fatalf("5 sessions: %+v %v", o5, ok)
	}
}

// A horizon not yet complete stays unknown; the in-progress session (after
// completedThrough) never counts.
func TestForwardOutcome_PendingAndInProgress_596(t *testing.T) {
	if _, ok := ForwardOutcome(fwdBars, day(10, 29), 100, 2, 5, day(11, 4)); ok {
		t.Fatal("the 5th session is still in progress: the 5-session outcome must stay pending")
	}
	if _, ok := ForwardOutcome(fwdBars, day(10, 29), 0, 2, 1, day(11, 5)); ok {
		t.Fatal("no reference close: no outcome")
	}
	o, ok := ForwardOutcome(fwdBars, day(10, 29), 100, 0, 1, day(11, 5))
	if !ok || !math.IsNaN(o.MFE) || !math.IsNaN(o.MAE) {
		t.Fatalf("no reference ATR: return only, ATR measures NaN: %+v", o)
	}
}

func TestSessionDatesInNewYork_596(t *testing.T) {
	// 2026-10-30 01:30 UTC is still 2026-10-29 21:30 in New York.
	if got := SessionDate(time.Date(2026, 10, 30, 1, 30, 0, 0, time.UTC)); !got.Equal(day(10, 29)) {
		t.Fatalf("session date %v", got)
	}
	// 15:59 ET: today's bar is not final yet.
	if got := CompletedThrough(time.Date(2026, 10, 30, 19, 59, 0, 0, time.UTC)); !got.Equal(day(10, 29)) {
		t.Fatalf("before the close: %v", got)
	}
	// 16:00 ET: it is.
	if got := CompletedThrough(time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)); !got.Equal(day(10, 30)) {
		t.Fatalf("at the close: %v", got)
	}
}

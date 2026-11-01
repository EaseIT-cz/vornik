package backtest

// GitHub #599: the daily replay of the v2.1 rules (trading-backtest-design.md §2 B).

import (
	"math"
	"strings"
	"testing"

	"vornik.io/vornik/internal/trading/rules"
)

var sessions = []string{"2024-01-02", "2024-01-03", "2024-01-04", "2024-01-05", "2024-01-08", "2024-01-09", "2024-01-10", "2024-01-11"}

// good is a scorecard that passes every daily gate, last close 100, ATR 1
// (so the stop distance is 3%, about 3 dollars).
func good() rules.Scorecard {
	return rules.Scorecard{Total: 4, LastClose: 100, SMA20: 98, SMA50: 95, SMA200: 90, RSI14: 60, ATR14: 1}
}

// fixture: AAPL qualifies on every session, SPY in trend, us RISK_ON, flat bars
// at 100 unless a test changes them.
func fixture() *History {
	h := &History{
		Sessions: sessions,
		Regions:  map[string]string{"AAPL": "us"},
		Bars:     map[string]map[string]Bar{},
		Scores:   map[string]map[string]rules.Scorecard{},
		Regimes:  map[string]map[string]rules.Regime{},
		Earnings: map[string][]string{"AAPL": {"2025-01-30"}},
	}
	for _, s := range sessions {
		h.Bars[s] = map[string]Bar{"AAPL": {Open: 100, High: 100.5, Low: 99.5, Close: 100}}
		h.Scores[s] = map[string]rules.Scorecard{"AAPL": good(), "SPY": good()}
		h.Regimes[s] = map[string]rules.Regime{"us": {Label: "RISK_ON", Score: 1, Components: 6}}
	}
	return h
}

func run(h *History, v Variant) Result {
	return Run(h, v, rules.DefaultPolicy(), Caps(), 0)
}

func TestEntryFillsAtTheSessionOpen(t *testing.T) {
	h := fixture()
	h.Bars[sessions[0]]["AAPL"] = Bar{Open: 101, High: 101.5, Low: 100.5, Close: 101}
	res := run(h, Variant{Name: "baseline"})
	if res.Entries == 0 {
		t.Fatal("no entry")
	}
	tr := res.Positions[0]
	if tr.EntrySession != sessions[0] || tr.Entry != 101 {
		t.Fatalf("entry %s at %v, want %s at the open 101", tr.EntrySession, tr.Entry, sessions[0])
	}
	if tr.Stop >= tr.Entry || tr.Qty < 1 {
		t.Fatalf("stop %v qty %v", tr.Stop, tr.Qty)
	}
}

func TestChaseRefusesAnOpenAboveTwoPercent(t *testing.T) {
	h := fixture()
	for _, s := range sessions {
		h.Bars[s]["AAPL"] = Bar{Open: 102.5, High: 103, Low: 102, Close: 102.5}
	}
	if res := run(h, Variant{Name: "baseline"}); res.Entries != 0 {
		t.Fatalf("entries %d, want 0: the open is 2.5%% above the last close", res.Entries)
	}
}

func TestStopHitIntradayExitsAtTheStop(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 99, High: 99.5, Low: 90, Close: 95}
	tr := closedFirst(t, run(h, Variant{Name: "baseline"}))
	if tr.Reason != "stop" || tr.Exit != tr.Stop || tr.ExitSession != sessions[1] {
		t.Fatalf("exit %+v, want the stop on %s", tr, sessions[1])
	}
	if math.Abs(tr.R-(-1)) > 1e-9 {
		t.Fatalf("R %v, want -1 without costs", tr.R)
	}
}

func TestGapBelowTheStopExitsAtTheOpen(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 90, High: 91, Low: 89, Close: 90}
	tr := closedFirst(t, run(h, Variant{Name: "baseline"}))
	if tr.Reason != "stop_gap" || tr.Exit != 90 || tr.R >= -1 {
		t.Fatalf("exit %+v, want the open 90 and R below -1", tr)
	}
}

func TestSMA50ExitAtTheNextOpen(t *testing.T) {
	h := fixture()
	sc := good()
	sc.LastClose, sc.SMA50 = 94, 95 // the previous session closed below SMA50
	h.Scores[sessions[2]]["AAPL"] = sc
	h.Bars[sessions[2]]["AAPL"] = Bar{Open: 101, High: 101, Low: 100, Close: 100}
	tr := closedFirst(t, run(h, Variant{Name: "baseline"}))
	if tr.Reason != "sma50" || tr.ExitSession != sessions[2] || tr.Exit != 101 {
		t.Fatalf("exit %+v, want sma50 at %s's open", tr, sessions[2])
	}
}

func TestCooldownThreeSessionsAfterAnyExit(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 99, High: 99.5, Low: 90, Close: 99}
	res := run(h, Variant{Name: "baseline"})
	// Exit on sessions[1]; sessions[2..4] are cooled down; re-entry on sessions[5].
	if len(res.Positions) < 2 || res.Positions[1].EntrySession != sessions[5] {
		var got []string
		for _, p := range res.Positions {
			got = append(got, p.EntrySession)
		}
		t.Fatalf("entries on %v, want the second on %s", got, sessions[5])
	}
}

func TestProfitTargetVariant(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 101, High: 110, Low: 100.5, Close: 108}
	tr := closedFirst(t, run(h, Variant{Name: "target_2r", TargetR: 2}))
	if tr.Reason != "target" || math.Abs(tr.R-2) > 1e-9 {
		t.Fatalf("exit %+v, want the 2R target", tr)
	}
}

func TestStopFirstWhenTargetAndStopShareABar(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 100, High: 110, Low: 90, Close: 100}
	if tr := closedFirst(t, run(h, Variant{Name: "target_2r", TargetR: 2})); tr.Reason != "stop" {
		t.Fatalf("exit %q, want stop (the conservative order)", tr.Reason)
	}
}

func TestATRTrailRaisesTheStopThenExits(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 100, High: 110, Low: 100, Close: 110} // trail to 110-3 = 107
	h.Bars[sessions[2]]["AAPL"] = Bar{Open: 109, High: 109, Low: 106, Close: 106} // hits 107
	tr := closedFirst(t, run(h, Variant{Name: "atr_trail_3", TrailATR: 3}))
	if tr.Reason != "trail" || tr.Exit != 107 || tr.R <= 0 {
		t.Fatalf("exit %+v, want the trailed stop at 107", tr)
	}
}

func TestTrailVariantIgnoresTheSMA50Exit(t *testing.T) {
	h := fixture()
	sc := good()
	sc.LastClose, sc.SMA50 = 94, 95
	h.Scores[sessions[2]]["AAPL"] = sc
	res := run(h, Variant{Name: "atr_trail_3", TrailATR: 3})
	for _, p := range res.Positions {
		if p.Reason == "sma50" {
			t.Fatal("the trail variant replaces the SMA50 exit")
		}
	}
}

func TestCostsScaleWithTheMultiplier(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 99, High: 99.5, Low: 90, Close: 95}
	one := closedFirst(t, Run(h, Variant{Name: "baseline"}, rules.DefaultPolicy(), Caps(), 1))
	two := closedFirst(t, Run(h, Variant{Name: "baseline"}, rules.DefaultPolicy(), Caps(), 2))
	if !(one.CostUSD > 0 && math.Abs(two.CostUSD-2*one.CostUSD) < 1e-9 && two.R < one.R && one.R < -1) {
		t.Fatalf("1x cost %v R %v, 2x cost %v R %v", one.CostUSD, one.R, two.CostUSD, two.R)
	}
}

func TestRiskOffGateAndItsVariant(t *testing.T) {
	h := fixture()
	for _, s := range sessions {
		h.Regimes[s]["us"] = rules.Regime{Label: "RISK_OFF", Score: -1, Components: 6}
	}
	if res := run(h, Variant{Name: "baseline"}); res.Entries != 0 {
		t.Fatalf("baseline entered %d times on RISK_OFF", res.Entries)
	}
	res := run(h, Variant{Name: "no_risk_off_gate", NoRiskOffGate: true})
	if res.Entries == 0 || res.Positions[0].RegimeLabel != "RISK_OFF" {
		t.Fatalf("no_risk_off_gate: %+v", res.Positions)
	}
}

func TestEarningsWindowAndUnavailableDates(t *testing.T) {
	h := fixture()
	h.Earnings["AAPL"] = []string{sessions[1]} // two sessions away from sessions[0], counting today as 1
	res := run(h, Variant{Name: "baseline"})
	if len(res.Positions) == 0 || res.Positions[0].EntrySession == sessions[0] {
		t.Fatal("entered inside the earnings window")
	}
	h.Earnings["AAPL"] = nil
	res = run(h, Variant{Name: "baseline"})
	if res.Positions[0].EntrySession != sessions[0] || res.EarningsNotModelled["AAPL"] == 0 {
		t.Fatalf("unavailable dates pass and are counted: %+v %v", res.Positions[0], res.EarningsNotModelled)
	}
}

func TestOpenPositionsAreNotScored(t *testing.T) {
	res := run(fixture(), Variant{Name: "baseline"})
	if len(res.Positions) != 1 || res.Positions[0].Closed() {
		t.Fatalf("positions %+v, want one open", res.Positions)
	}
	if got := res.EntryResults(); got[0].Status != "open" {
		t.Fatalf("status %q, want open", got[0].Status)
	}
}

func TestLoadHistory(t *testing.T) {
	in := `{"kind":"meta","symbols":{"AAPL":"us"}}
{"kind":"earnings","symbol":"AAPL","dates":null}
{"kind":"regime","session":"2024-01-02","region":"us","score":1,"label":"RISK_ON","components":6,"stale":false}
{"kind":"bar","session":"2024-01-02","symbol":"SPY","open":1,"high":1,"low":1,"close":1}
{"kind":"bar","session":"2024-01-02","symbol":"AAPL","open":100,"high":101,"low":99,"close":100.5}
{"kind":"scorecard","session":"2024-01-02","symbol":"AAPL","region":"us","total":4,"trend":2,"momentum":1,"macro":1,"last_close":100,"sma20":98,"sma50":95,"sma200":90,"rsi14":60,"atr14":1}
{"kind":"scorecard","session":"2024-01-02","symbol":"MSFT","region":"us","total":4,"last_close":100,"sma20":null,"sma50":95,"sma200":90,"rsi14":60,"atr14":1}
`
	h, err := Load(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Sessions) != 1 || h.Regions["AAPL"] != "us" || h.Bars["2024-01-02"]["AAPL"].High != 101 {
		t.Fatalf("%+v", h)
	}
	if sc := h.Scores["2024-01-02"]["AAPL"]; sc.Total != 4 || sc.SMA50 != 95 || sc.ATR14 != 1 {
		t.Fatalf("scorecard %+v", sc)
	}
	if _, ok := h.Scores["2024-01-02"]["MSFT"]; ok {
		t.Fatal("a scorecard with a null value is unusable and must be dropped")
	}
	if r := h.Regimes["2024-01-02"]["us"]; r.Label != "RISK_ON" || r.Components != 6 {
		t.Fatalf("regime %+v", r)
	}
	if v, ok := h.Earnings["AAPL"]; !ok || v != nil {
		t.Fatal("null earnings must load as present and nil (unavailable)")
	}
}

func closedFirst(t *testing.T, res Result) Position {
	t.Helper()
	for _, p := range res.Positions {
		if p.Closed() {
			return p
		}
	}
	t.Fatalf("no closed position in %+v", res.Positions)
	return Position{}
}

package trading

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/trading/rules"
)

// GitHub #598 step 1: the rule engine's inputs come from the strategist's
// own tool calls; a call it did not make, or one that failed, stays nil.

// hourBars alternates +up and -down moves from start: a rising market with
// an RSI near 100*up/(up+down).
func hourBars(n int, start, up, down float64) string {
	var b strings.Builder
	b.WriteString(`{"bars":[`)
	v := start
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
			if i%2 == 1 {
				v += up
			} else {
				v -= down
			}
		}
		fmt.Fprintf(&b, `{"close":%g}`, v)
	}
	b.WriteString("]}")
	return b.String()
}

func TestShadowInputs_FromToolCalls_598(t *testing.T) {
	now := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC) // a Monday
	sc := `{"total":4,"trend":2,"momentum":1,"macro":1,"last_close":100,"sma20":98,"sma50":95,"sma200":90,"rsi14":55,"atr14":2}`
	calls := []ToolCall{
		{Name: "mcp__broker__get_account_summary", Output: `{"realised_pl_day_usd":-40,"positions":[{"symbol":"MSFT","qty":5,"market_value":2100}]}`, OK: true},
		{Name: "mcp__broker__get_orders", Output: `{"open":[{"symbol":"JPM","action":"BUY","client_tag":"k1"},{"symbol":"MSFT","action":"SELL","client_tag":"k0_stop"}]}`, OK: true},
		{Name: "mcp__ta__scorecard", Input: `{"symbol":"SPY","region":"us"}`, Output: `{"total":1,"last_close":510,"sma20":505,"sma50":505,"sma200":500,"rsi14":55,"atr14":5}`, OK: true},
		{Name: "mcp__ta__scorecard", Input: `{"symbol":"AAPL","region":"us"}`, Output: sc, OK: true},
		{Name: "mcp__ta__scorecard", Input: `{"symbol":"MSFT","region":"us"}`, Output: sc, OK: true},
		{Name: "mcp__ta__regime", Input: `{"region":"us"}`, Output: `{"score":2,"label":"risk_on","stale":false,"component_count":6}`, OK: true},
		{Name: "mcp__broker__get_historical_bars", Input: `{"symbol":"AAPL","bar_size":"1 hour","duration":"10 D"}`, Output: hourBars(30, 90, 0.5, 0.3), OK: true},
		{Name: "mcp__broker__get_quote", Input: `{"symbol":"AAPL"}`, Output: `{"bid":99.95,"ask":100.05,"delayed":false}`, OK: true},
		{Name: "mcp__news__fundamentals_snapshot", Input: `{"symbol":"AAPL"}`, Output: `{"next_earnings_date":"2026-11-20"}`, OK: true},
		{Name: "mcp__news__fundamentals_snapshot", Input: `{"symbol":"GOOGL"}`, Output: `{"error":"x"}`, OK: false},
	}
	in := ShadowInputs(calls, []string{"AAPL", "MSFT", "GOOGL"}, now, 0.01)
	if !in.StateOK || !in.Held["MSFT"] || !in.Pending["JPM"] || in.Pending["MSFT"] {
		t.Fatalf("state %+v", in)
	}
	if in.RealizedLossTodayUSD != 40 || in.GrossExposureUSD != 2100 || in.PositionsByRegion["us"] != 1 {
		t.Fatalf("account %+v", in)
	}
	if r := in.Regimes["us"]; r.Label != "RISK_ON" || r.Components != 6 {
		t.Fatalf("regime %+v", r)
	}
	aapl := in.Candidates[0]
	if aapl.Hourly == nil || aapl.Hourly.Close <= aapl.Hourly.SMA20 || aapl.Hourly.RSI14 < 55 || aapl.Hourly.RSI14 > 70 || aapl.Quote == nil || aapl.EarningsInSessions == nil || *aapl.EarningsInSessions != 15 {
		b, _ := json.Marshal(aapl)
		t.Fatalf("AAPL %s", b)
	}
	if in.Candidates[2].Score != nil || in.Candidates[2].EarningsInSessions != nil {
		t.Fatal("GOOGL: no scorecard, and a failed fundamentals call stays unknown")
	}
	d := rules.Decide(in, rules.DefaultPolicy(), rules.Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8, MaxGrossExposureUSD: 15000, DailyLossPauseUSD: 150})
	gates := map[string]string{}
	for _, r := range d.Funnel {
		gates[r.Symbol] = r.Gate
	}
	if gates["AAPL"] != "none" || gates["MSFT"] != "held" || gates["GOOGL"] != "insufficient_inputs" {
		t.Fatalf("gates %v", gates)
	}
}

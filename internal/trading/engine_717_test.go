package trading

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/trading/rules"
)

// GitHub #717 (#598 step 2): the rule engine decides the opens. The model's
// open proposals are replaced by the engine's; its closes and adds stay.

const modelResult717 = `{"proposals":[
  {"symbol":"GOOGL","intent":"open","action":"BUY","qty":3,"limit_price":300},
  {"symbol":"SHEL","intent":"close","action":"SELL","qty":26,"limit_price":95},
  {"symbol":"SAP","intent":"add","action":"BUY","qty":2,"limit_price":230}],
 "has_proposals":true,
 "holdings_review":[{"symbol":"SHEL","last_close":95,"sma50":96,"verdict":"close"}],
 "entry_funnel":[
  {"symbol":"GOOGL","intent":"open","status":"proposed","gate":"none","reason":"model"},
  {"symbol":"SAP","intent":"add","status":"proposed","gate":"none","reason":"model"}],
 "rationale":"kept"}`

func engineDecision717() (rules.Decision, rules.Inputs) {
	sc := &rules.Scorecard{Total: 5, Trend: 2, Momentum: 2, Macro: 1, LastClose: 1000, SMA20: 990, SMA50: 950, SMA200: 900, RSI14: 60, ATR14: 15}
	in := rules.Inputs{
		Regimes:    map[string]rules.Regime{"eu": {Label: "RISK_ON", Score: 1, Components: 5}},
		Candidates: []rules.Candidate{{Symbol: "ASML", Region: "eu", Score: sc}},
	}
	d := rules.Decision{
		Entries: []rules.Entry{{Symbol: "ASML", Size: rules.Sizing{Qty: 2, Entry: 1000.5, Stop: 970.5}, Total: 5}},
		Funnel: []rules.FunnelRow{{Symbol: "ASML", Intent: "open", Status: "proposed", Gate: "none"},
			{Symbol: "GOOGL", Intent: "open", Status: "rejected", Gate: "hourly_trend"}},
	}
	return d, in
}

type prop717 struct {
	Symbol        string  `json:"symbol"`
	Intent        string  `json:"intent"`
	Action        string  `json:"action"`
	OrderType     string  `json:"order_type"`
	HoldingState  string  `json:"holding_state"`
	Region        string  `json:"region"`
	Qty           float64 `json:"qty"`
	LimitPrice    float64 `json:"limit_price"`
	StopLossPrice float64 `json:"stop_loss_price"`
	Conviction    float64 `json:"conviction"`
	Scorecard     *struct{ Total, Trend, Momentum, Macro int }
	Regime        *struct {
		Score          *int
		Label          string
		Stale          bool
		ComponentCount int `json:"component_count"`
	}
}

func decode717(t *testing.T, b []byte) (map[string]json.RawMessage, []map[string]any, []map[string]any) {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &obj))
	var props, funnel []map[string]any
	require.NoError(t, json.Unmarshal(obj["proposals"], &props))
	require.NoError(t, json.Unmarshal(obj["entry_funnel"], &funnel))
	return obj, props, funnel
}

func TestRewriteForEngine_717(t *testing.T) {
	d, in := engineDecision717()
	out, err := RewriteForEngine([]byte(modelResult717), d, in)
	require.NoError(t, err)
	obj, props, funnel := decode717(t, out)

	var syms []string
	for _, p := range props {
		syms = append(syms, p["symbol"].(string)+"/"+p["intent"].(string))
	}
	assert.ElementsMatch(t, []string{"SHEL/close", "SAP/add", "ASML/open"}, syms, "model open dropped, close and add kept, engine open added")
	assert.JSONEq(t, `true`, string(obj["has_proposals"]))
	assert.JSONEq(t, `"kept"`, string(obj["rationale"]), "other fields survive")

	var engine prop717
	raw, _ := json.Marshal(props[len(props)-1])
	require.NoError(t, json.Unmarshal(raw, &engine))
	assert.Equal(t, "ASML", engine.Symbol)
	assert.Equal(t, "BUY", engine.Action)
	assert.Equal(t, "LMT", engine.OrderType)
	assert.Equal(t, "flat", engine.HoldingState)
	assert.Equal(t, "eu", engine.Region)
	assert.InDelta(t, 2, engine.Qty, 0)
	assert.InDelta(t, 1000.5, engine.LimitPrice, 1e-9)
	assert.InDelta(t, 970.5, engine.StopLossPrice, 1e-9)
	assert.InDelta(t, 5.0/6, engine.Conviction, 1e-9)
	require.NotNil(t, engine.Scorecard)
	assert.Equal(t, struct{ Total, Trend, Momentum, Macro int }{5, 2, 2, 1}, *engine.Scorecard)
	require.NotNil(t, engine.Regime)
	require.NotNil(t, engine.Regime.Score)
	assert.Equal(t, 1, *engine.Regime.Score)
	assert.Equal(t, "RISK_ON", engine.Regime.Label)
	assert.Equal(t, 5, engine.Regime.ComponentCount)

	var rows []string
	for _, r := range funnel {
		rows = append(rows, r["symbol"].(string)+"/"+r["intent"].(string)+"/"+r["gate"].(string))
	}
	assert.ElementsMatch(t, []string{"SAP/add/none", "ASML/open/none", "GOOGL/open/hourly_trend"}, rows,
		"the model's open rows are replaced by the engine's; add rows stay")
	for _, r := range funnel {
		if r["symbol"] == "ASML" {
			in := r["inputs"].(map[string]any)
			assert.Equal(t, 1000.0, in["last_close"], "forward outcomes need the reference close")
			assert.Equal(t, 15.0, in["atr14"])
		}
	}
}

func TestRewriteForEngine_NoDecisionDropsModelOpens_717(t *testing.T) {
	out, err := RewriteForEngine([]byte(modelResult717), rules.Decision{}, rules.Inputs{})
	require.NoError(t, err)
	obj, props, _ := decode717(t, out)
	for _, p := range props {
		assert.NotEqual(t, "open", p["intent"], "an engine error must never let a model open through")
	}
	assert.JSONEq(t, `true`, string(obj["has_proposals"]), "the close and add remain")

	onlyOpen := `{"proposals":[{"symbol":"GOOGL","intent":"open","action":"BUY"}],"has_proposals":true,"entry_funnel":[]}`
	out, err = RewriteForEngine([]byte(onlyOpen), rules.Decision{}, rules.Inputs{})
	require.NoError(t, err)
	obj, props, _ = decode717(t, out)
	assert.Empty(t, props)
	assert.JSONEq(t, `false`, string(obj["has_proposals"]))
}

func TestDropOpensBelowSMA50_717(t *testing.T) {
	d, in := engineDecision717()
	b, err := RewriteForEngine([]byte(modelResult717), d, in)
	require.NoError(t, err)
	out, dropped := DropOpensBelowSMA50(b, map[string]float64{"ASML": 1001})
	assert.Equal(t, []string{"ASML"}, dropped)
	_, props, funnel := decode717(t, out)
	for _, p := range props {
		assert.NotEqual(t, "ASML", p["symbol"])
	}
	found := false
	for _, r := range funnel {
		if r["symbol"] == "ASML" {
			found = true
			assert.Equal(t, "rejected", r["status"])
			assert.Equal(t, "daily_trend", r["gate"])
		}
	}
	assert.True(t, found)
	// An SMA50 at or below the limit keeps the open.
	out, dropped = DropOpensBelowSMA50(b, map[string]float64{"ASML": 1000.5})
	assert.Empty(t, dropped)
	assert.Equal(t, string(b), string(out))
	// An unknown SMA50 drops it: no layer could check the trend floor.
	_, dropped = DropOpensBelowSMA50(b, map[string]float64{})
	assert.Equal(t, []string{"ASML"}, dropped)
}

func TestValidFetched_RecordedResponses_717(t *testing.T) {
	quote, err := os.ReadFile("testdata/get_quote_717.json")
	require.NoError(t, err)
	fund, err := os.ReadFile("testdata/fundamentals_snapshot_717.json")
	require.NoError(t, err)
	assert.True(t, ValidFetched("get_quote", string(quote)))
	assert.True(t, ValidFetched("fundamentals_snapshot", string(fund)))
	bars := `{"bar_size":"1 hour","bars":[` + repeatBar(21) + `]}`
	assert.True(t, ValidFetched("get_historical_bars", bars))

	for name, c := range map[string][2]string{
		"no bid":           {"get_quote", `{"bid":0,"ask":10}`},
		"crossed":          {"get_quote", `{"bid":11,"ask":10}`},
		"unavailable":      {"get_quote", `{"bid":9.9,"ask":10,"available":false}`},
		"not json":         {"get_quote", `{"bid":`},
		"20 bars":          {"get_historical_bars", `{"bars":[` + repeatBar(20) + `]}`},
		"zero close":       {"get_historical_bars", `{"bars":[` + repeatBar(20) + `,{"close":0}]}`},
		"no earnings info": {"fundamentals_snapshot", `{"symbol":"X","next_earnings_date":null,"days_since_last_earnings":null}`},
		"unknown tool":     {"place_order", `{}`},
	} {
		assert.False(t, ValidFetched(c[0], c[1]), name)
	}
}

func repeatBar(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += `{"close":100}`
	}
	return s
}

package trading

import (
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/trading/rules"
)

// engine.go applies the rule engine's decision to the strategist's result
// (GitHub #717, #598 step 2; trading entry-policy design, Amendment
// 2026-11-10 #717 §2 B). The model's open proposals and open funnel rows are
// replaced by the engine's; its closes and adds stay, with their own checks.

// engineProposal is one engine open in the strategist's proposal schema.
type engineProposal struct {
	Symbol        string       `json:"symbol"`
	Intent        string       `json:"intent"`
	Action        string       `json:"action"`
	Qty           float64      `json:"qty"`
	Conviction    float64      `json:"conviction"`
	OrderType     string       `json:"order_type"`
	LimitPrice    float64      `json:"limit_price"`
	StopLossPrice float64      `json:"stop_loss_price"`
	HoldingState  string       `json:"holding_state"`
	Region        string       `json:"region"`
	Scorecard     engineScore  `json:"scorecard"`
	Regime        engineRegime `json:"regime"`
	Rationale     string       `json:"rationale"`
	Source        string       `json:"source"`
}

type engineScore struct {
	Total    int `json:"total"`
	Trend    int `json:"trend"`
	Momentum int `json:"momentum"`
	Macro    int `json:"macro"`
}

type engineRegime struct {
	Score          int    `json:"score"`
	Label          string `json:"label"`
	Stale          bool   `json:"stale"`
	ComponentCount int    `json:"component_count"`
}

type engineFunnelRow struct {
	Symbol string         `json:"symbol"`
	Intent string         `json:"intent"`
	Status string         `json:"status"`
	Gate   string         `json:"gate"`
	Reason string         `json:"reason"`
	Inputs map[string]any `json:"inputs"`
}

// RewriteForEngine returns original with the model's intent=open proposals
// and open funnel rows replaced by d's. An empty decision (the engine
// failed) still drops the model's opens: they are never a fallback.
func RewriteForEngine(original []byte, d rules.Decision, in rules.Inputs) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(original, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("strategist result is not a JSON object: %v", err)
	}
	props, err := keepRows(obj["proposals"], func(r map[string]json.RawMessage) bool { return intentOf(r) != "open" })
	if err != nil {
		return nil, fmt.Errorf("proposals: %w", err)
	}
	funnel, err := keepRows(obj["entry_funnel"], func(r map[string]json.RawMessage) bool { return intentOf(r) != "open" })
	if err != nil {
		return nil, fmt.Errorf("entry_funnel: %w", err)
	}

	cands := make(map[string]rules.Candidate, len(in.Candidates))
	for _, c := range in.Candidates {
		cands[c.Symbol] = c
	}
	for _, e := range d.Entries {
		c, ok := cands[e.Symbol]
		if !ok || c.Score == nil {
			continue // the engine sized it, so this cannot happen; never emit an unscored open
		}
		r := in.Regimes[c.Region]
		p := engineProposal{
			Symbol: e.Symbol, Intent: "open", Action: "BUY", Qty: e.Size.Qty,
			Conviction: min(1, float64(e.Total)/6), OrderType: "LMT",
			LimitPrice: e.Size.Entry, StopLossPrice: e.Size.Stop, HoldingState: "flat", Region: c.Region,
			Scorecard: engineScore{c.Score.Total, c.Score.Trend, c.Score.Momentum, c.Score.Macro},
			Regime:    engineRegime{r.Score, r.Label, r.Stale, r.Components},
			Rationale: fmt.Sprintf("rule engine: total %d, last close %.2f > sma20 %.2f and sma50 %.2f, rsi14 %.1f; limit %.2f, stop %.2f",
				c.Score.Total, c.Score.LastClose, c.Score.SMA20, c.Score.SMA50, c.Score.RSI14, e.Size.Entry, e.Size.Stop),
			Source: "rule_engine",
		}
		b, _ := json.Marshal(p)
		props = append(props, b)
	}
	for _, f := range d.Funnel {
		// last_close and atr14 are what the #596 forward outcomes are
		// measured against (recordFunnel's ref_close and ref_atr).
		inputs := map[string]any{}
		if c, ok := cands[f.Symbol]; ok && c.Score != nil {
			inputs["last_close"], inputs["atr14"], inputs["total"] = c.Score.LastClose, c.Score.ATR14, c.Score.Total
		}
		b, _ := json.Marshal(engineFunnelRow{Symbol: f.Symbol, Intent: f.Intent, Status: f.Status, Gate: f.Gate,
			Reason: "rule engine", Inputs: inputs})
		funnel = append(funnel, b)
	}
	return withRows(obj, props, funnel)
}

// DropOpenProposals removes every intent=open proposal and nothing else: the
// engine-error path, where the model's opens must not go through and the
// rest of the result may be malformed.
func DropOpenProposals(original []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(original, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("strategist result is not a JSON object: %v", err)
	}
	props, err := keepRows(obj["proposals"], func(r map[string]json.RawMessage) bool { return intentOf(r) != "open" })
	if err != nil {
		props = []json.RawMessage{} // unreadable proposals: none go through
	}
	if obj["proposals"], err = json.Marshal(props); err != nil {
		return nil, err
	}
	obj["has_proposals"] = json.RawMessage(fmt.Sprint(len(props) > 0))
	return json.Marshal(obj)
}

// DropOpensBelowSMA50 removes opens whose limit is below the daemon's own
// daily SMA50 (the value the enforcing entry_gate_consistent verifier reads),
// or whose SMA50 is unknown (fail closed: no layer could check it), and marks
// their funnel rows daily_trend. b is returned unchanged when nothing is
// dropped.
func DropOpensBelowSMA50(b []byte, sma50 map[string]float64) ([]byte, []string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return b, nil
	}
	var dropped []string
	props, err := keepRows(obj["proposals"], func(r map[string]json.RawMessage) bool {
		if intentOf(r) != "open" {
			return true
		}
		sym := strings.ToUpper(rawJSONString(r["symbol"]))
		var limit float64
		_ = json.Unmarshal(r["limit_price"], &limit)
		if s, ok := sma50[sym]; !ok || s <= 0 || limit < s {
			dropped = append(dropped, sym)
			return false
		}
		return true
	})
	if err != nil || len(dropped) == 0 {
		return b, nil
	}
	var rows []map[string]any
	_ = json.Unmarshal(obj["entry_funnel"], &rows)
	funnel := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		if sym, _ := r["symbol"].(string); r["intent"] == "open" && contains(dropped, strings.ToUpper(sym)) {
			r["status"], r["gate"], r["reason"] = "rejected", "daily_trend", "limit below the daily SMA50"
		}
		rb, _ := json.Marshal(r)
		funnel = append(funnel, rb)
	}
	out, err := withRows(obj, props, funnel)
	if err != nil {
		return b, nil
	}
	return out, dropped
}

// keepRows decodes a JSON array of objects and keeps the rows keep accepts.
// An absent array is empty.
func keepRows(raw json.RawMessage, keep func(map[string]json.RawMessage) bool) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		var r map[string]json.RawMessage
		if json.Unmarshal(row, &r) != nil || keep(r) {
			out = append(out, row)
		}
	}
	return out, nil
}

func withRows(obj map[string]json.RawMessage, props, funnel []json.RawMessage) ([]byte, error) {
	var err error
	if obj["proposals"], err = json.Marshal(props); err != nil {
		return nil, err
	}
	if obj["entry_funnel"], err = json.Marshal(funnel); err != nil {
		return nil, err
	}
	obj["has_proposals"] = json.RawMessage(fmt.Sprint(len(props) > 0))
	return json.Marshal(obj)
}

func intentOf(r map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(r["intent"], &s)
	return strings.ToLower(strings.TrimSpace(s))
}

func rawJSONString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ValidFetched reports whether a daemon-fetched tool output is complete
// enough to decide on (#717 §2 B.2). Anything else is a failed fetch.
func ValidFetched(tool, out string) bool {
	switch tool {
	case "get_quote":
		var q struct {
			Bid, Ask  float64
			Available *bool `json:"available"`
		}
		if json.Unmarshal([]byte(out), &q) != nil {
			return false
		}
		return q.Bid > 0 && q.Ask > 0 && q.Bid <= q.Ask && (q.Available == nil || *q.Available)
	case "get_historical_bars":
		var b struct {
			Bars []struct {
				Close float64 `json:"close"`
			} `json:"bars"`
		}
		if json.Unmarshal([]byte(out), &b) != nil || len(b.Bars) < 21 {
			return false
		}
		for _, bar := range b.Bars {
			if bar.Close <= 0 {
				return false
			}
		}
		return true
	case "fundamentals_snapshot":
		var f struct {
			Next      *string `json:"next_earnings_date"`
			SinceLast *int    `json:"days_since_last_earnings"`
		}
		if json.Unmarshal([]byte(out), &f) != nil {
			return false
		}
		return f.Next != nil || f.SinceLast != nil
	}
	return false
}

package trading

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"vornik.io/vornik/internal/trading/rules"
)

// shadow.go builds the rule engine's inputs from the strategist step's own
// tool calls (GitHub #598 step 1; trading entry-policy design, Amendment
// 2026-10-31 §2 B). No new call is made: what the model did not fetch, or
// fetched and failed, stays nil, and the engine records insufficient_inputs.

// ShadowInputs reads one accepted strategist step's tool calls. universe is
// the entry universe; tick is the price increment used for sizing.
func ShadowInputs(calls []ToolCall, universe []string, now time.Time, tick float64) rules.Inputs {
	ev := collect(calls)
	in := rules.Inputs{
		InSession:  true,
		StateOK:    ev.snapshot,
		Held:       map[string]bool{},
		Pending:    map[string]bool{},
		CooledDown: map[string]bool{},
		Regimes:    map[string]rules.Regime{},
	}
	for sym := range ev.held {
		in.Held[sym] = true
	}
	in.Positions = len(ev.held)
	t := shadowTools{regions: map[string]string{}, hourly: map[string]*rules.Hourly{},
		quotes: map[string]*rules.Quote{}, earnings: map[string]*int{}}
	for _, c := range calls {
		if c.OK {
			t.read(c, &in, now)
		}
	}
	regions, hourly, quotes, earnings := t.regions, t.hourly, t.quotes, t.earnings
	in.PositionsByRegion = map[string]int{}
	for sym := range ev.held {
		in.PositionsByRegion[regions[sym]]++
	}
	if sc, ok := ev.scorecards["SPY"]; ok {
		s := toRulesScore(sc)
		in.SPY = &s
	}
	for _, sym := range universe {
		c := rules.Candidate{Symbol: sym, Region: regions[sym], Hourly: hourly[sym], Quote: quotes[sym], Tick: tick, EarningsInSessions: earnings[sym]}
		if sc, ok := ev.scorecards[sym]; ok {
			s := toRulesScore(sc)
			c.Score = &s
		}
		in.Candidates = append(in.Candidates, c)
	}
	return in
}

func toRulesScore(sc scorecardOut) rules.Scorecard {
	i := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	f := func(p *float64) float64 {
		if p == nil {
			return math.NaN()
		}
		return *p
	}
	return rules.Scorecard{Total: i(sc.Total), Trend: i(sc.Trend), Momentum: i(sc.Momentum), Macro: i(sc.Macro),
		LastClose: f(sc.LastClose), SMA20: f(sc.SMA20), SMA50: f(sc.SMA50), SMA200: f(sc.SMA200), RSI14: f(sc.RSI14), ATR14: f(sc.ATR14)}
}

func strField(raw, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func readAccount(out string, in *rules.Inputs) {
	var a struct {
		RealisedPLDay float64 `json:"realised_pl_day_usd"`
		Positions     []struct {
			Qty         float64 `json:"qty"`
			MarketValue float64 `json:"market_value"`
		} `json:"positions"`
	}
	if json.Unmarshal([]byte(out), &a) != nil {
		return
	}
	if a.RealisedPLDay < 0 {
		in.RealizedLossTodayUSD = -a.RealisedPLDay
	}
	for _, p := range a.Positions {
		if p.Qty != 0 {
			in.GrossExposureUSD += math.Abs(p.MarketValue)
		}
	}
}

// readPending marks symbols with a working BUY parent order (a bracket's
// "_stop" and "_tp" children are not entries).
func readPending(out string, pending map[string]bool) {
	var o struct {
		Open []struct {
			Symbol    string `json:"symbol"`
			Action    string `json:"action"`
			ClientTag string `json:"client_tag"`
		} `json:"open"`
	}
	if json.Unmarshal([]byte(out), &o) != nil {
		return
	}
	for _, x := range o.Open {
		if strings.EqualFold(x.Action, "BUY") && !strings.HasSuffix(x.ClientTag, "_stop") && !strings.HasSuffix(x.ClientTag, "_tp") {
			pending[strings.ToUpper(x.Symbol)] = true
		}
	}
}

func readRegime(c ToolCall, regimes map[string]rules.Regime) {
	var out struct {
		Region         string `json:"region"`
		Score          *int   `json:"score"`
		Label          string `json:"label"`
		Stale          bool   `json:"stale"`
		ComponentCount int    `json:"component_count"`
	}
	if json.Unmarshal([]byte(c.Output), &out) != nil || out.Score == nil {
		return
	}
	region := strings.ToLower(upperField(c.Input, "region"))
	if region == "" {
		region = strings.ToLower(out.Region)
	}
	regimes[region] = rules.Regime{Label: strings.ToUpper(out.Label), Score: *out.Score, Stale: out.Stale, Components: out.ComponentCount}
}

// hourlyFromBars computes the last 1h bar's close, SMA20 and Wilder RSI14.
// Fewer than 21 bars is no reading.
func hourlyFromBars(out string) *rules.Hourly {
	var o struct {
		Bars []struct {
			Close float64 `json:"close"`
		} `json:"bars"`
	}
	if json.Unmarshal([]byte(out), &o) != nil || len(o.Bars) < 21 {
		return nil
	}
	closes := make([]float64, len(o.Bars))
	for i, b := range o.Bars {
		closes[i] = b.Close
	}
	n := len(closes)
	sum := 0.0
	for _, v := range closes[n-20:] {
		sum += v
	}
	return &rules.Hourly{Close: closes[n-1], SMA20: sum / 20, RSI14: wilderRSI(closes, 14)}
}

func wilderRSI(closes []float64, period int) float64 {
	var gain, loss float64
	for i := 1; i <= period; i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	gain /= float64(period)
	loss /= float64(period)
	for i := period + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		g, l := math.Max(d, 0), math.Max(-d, 0)
		gain = (gain*float64(period-1) + g) / float64(period)
		loss = (loss*float64(period-1) + l) / float64(period)
	}
	if loss == 0 {
		return 100
	}
	return 100 - 100/(1+gain/loss)
}

func readQuote(out string) *rules.Quote {
	var q struct {
		Bid     float64 `json:"bid"`
		Ask     float64 `json:"ask"`
		Delayed bool    `json:"delayed"`
	}
	if json.Unmarshal([]byte(out), &q) != nil {
		return nil
	}
	return &rules.Quote{Bid: q.Bid, Ask: q.Ask, Delayed: q.Delayed}
}

// earningsSessions counts US sessions from today (counting as 1) to the
// next earnings date. A null next date with the last release at most 45
// days ago is known, far timing; anything else unknown is nil.
func earningsSessions(out string, now time.Time) *int {
	var f struct {
		Next      *string `json:"next_earnings_date"`
		SinceLast *int    `json:"days_since_last_earnings"`
	}
	if json.Unmarshal([]byte(out), &f) != nil {
		return nil
	}
	if f.Next == nil {
		if f.SinceLast != nil && *f.SinceLast <= 45 {
			far := 99
			return &far
		}
		return nil
	}
	next, err := time.Parse(time.DateOnly, *f.Next)
	if err != nil {
		return nil
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	n := 0
	for d := day; !d.After(next); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday && !IsUSMarketHoliday(d) {
			n++
		}
	}
	return &n
}

// shadowTools collects the per-symbol readings of one step's tool calls.
type shadowTools struct {
	regions  map[string]string
	hourly   map[string]*rules.Hourly
	quotes   map[string]*rules.Quote
	earnings map[string]*int
}

// read takes one successful tool call.
func (t *shadowTools) read(c ToolCall, in *rules.Inputs, now time.Time) {
	sym := upperField(c.Input, "symbol")
	switch bareTool(c.Name) {
	case "get_account_summary":
		readAccount(c.Output, in)
	case "get_orders":
		readPending(c.Output, in.Pending)
	case "regime":
		readRegime(c, in.Regimes)
	}
	if sym == "" {
		return
	}
	switch bareTool(c.Name) {
	case "scorecard":
		if r := strings.ToLower(upperField(c.Input, "region")); r != "" {
			t.regions[sym] = r
		}
	case "get_historical_bars":
		if strings.EqualFold(strField(c.Input, "bar_size"), "1 hour") {
			if h := hourlyFromBars(c.Output); h != nil {
				t.hourly[sym] = h
			}
		}
	case "get_quote":
		if q := readQuote(c.Output); q != nil {
			t.quotes[sym] = q
		}
	case "fundamentals_snapshot":
		if n := earningsSessions(c.Output, now); n != nil {
			t.earnings[sym] = n
		}
	}
}

package trading

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// funnel.go — the strategist's entry funnel, recorded and scored on its own
// (GitHub #596; trading entry-policy design, Amendment 2026-10-31). Pure
// functions only: parsing the funnel from the accepted strategist output,
// normalising the model's free-text gate names, and computing a candidate's
// forward outcome from daily bars. Measurement only: nothing here can block
// a trade.

// FunnelRow is one entry_funnel row as the strategist emitted it.
type FunnelRow struct {
	Symbol string          `json:"symbol"`
	Intent string          `json:"intent"`
	Status string          `json:"status"`
	Gate   string          `json:"gate"`
	Reason string          `json:"reason"`
	Inputs json.RawMessage `json:"inputs"`
}

// ParseFunnel returns the entry_funnel rows of a strategist result. A
// result without a funnel, or with one that does not parse, returns nil
// and ok=false; rows without a symbol are dropped.
func ParseFunnel(result []byte) (rows []FunnelRow, ok bool) {
	var out struct {
		EntryFunnel []FunnelRow `json:"entry_funnel"`
	}
	if err := json.Unmarshal(result, &out); err != nil || out.EntryFunnel == nil {
		return nil, false
	}
	for _, r := range out.EntryFunnel {
		if strings.TrimSpace(r.Symbol) != "" {
			rows = append(rows, r)
		}
	}
	return rows, true
}

// gateSpellings maps every spelling the strategist has used (measured
// 2026-10-31, design §As built) to one gate name. Anything else is "other",
// and the raw spelling is kept beside it.
var gateSpellings = map[string]string{
	"regime":                "regime",
	"spy_regime":            "spy_regime",
	"scorecard_total":       "scorecard",
	"scorecard_floor":       "scorecard",
	"daily_trend":           "daily_trend",
	"trend":                 "daily_trend",
	"daily_trend_alignment": "daily_trend",
	"held":                  "held",
	"held_already":          "held",
	"already_held":          "held",
	"held_add_eligibility":  "held",
	"rsi_add_band":          "rsi_band",
	"add_rsi_band":          "rsi_band",
	"rsi_floor":             "rsi_band",
	"rsi":                   "rsi_band",
	"earnings_proximity":    "earnings",
	"rank":                  "rank",
	"none":                  "none",
	"data_quality":          "data_quality",
	// The rule engine's own gate names (rules.Decide), which are already the
	// fixed names; in engine mode the funnel records them (GitHub #717).
	"scorecard":               "scorecard",
	"earnings":                "earnings",
	"cooldown":                "cooldown",
	"loss_pause":              "loss_pause",
	"slots":                   "slots",
	"hourly_trend":            "hourly_trend",
	"quote":                   "quote",
	"chase":                   "chase",
	"stop_too_wide":           "stop_too_wide",
	"size_zero":               "size_zero",
	"size_below_min_notional": "size_below_min_notional",
	"insufficient_inputs":     "insufficient_inputs",
}

// NormalizeGate returns the gate's fixed name, or "other".
func NormalizeGate(raw string) string {
	if g, ok := gateSpellings[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return g
	}
	return "other"
}

// RefPrices reads last_close and atr14 from a row's inputs; a missing or
// non-positive value is 0.
func RefPrices(inputs json.RawMessage) (lastClose, atr float64) {
	var in struct {
		LastClose float64 `json:"last_close"`
		ATR14     float64 `json:"atr14"`
	}
	_ = json.Unmarshal(inputs, &in)
	return math.Max(in.LastClose, 0), math.Max(in.ATR14, 0)
}

// DailyBar is one completed or in-progress daily bar.
type DailyBar struct {
	Session time.Time // the session date, at midnight UTC
	High    float64
	Low     float64
	Close   float64
}

// Outcome is a candidate's result over one horizon. ATR measures are NaN
// when the candidate has no reference ATR.
type Outcome struct {
	Return float64
	MFE    float64
	MAE    float64
}

// Baseline is the reference a candidate's outcomes are measured from, taken
// from the same series as the forward bars (#719). refClose is the series'
// close of the session a tick's scorecard last_close describes: the last
// session completed at observedAt (the scorecard never uses the in-progress
// candle; the observed session's own close is set after the tick). atr is
// the series' Wilder ATR14 through that session, 0 when fewer than 15 bars
// lead up to it. The session is the last NYSE trading day completed at
// observedAt (weekends and IsUSMarketHoliday skipped), compared with
// DailyBar.Session in the same form: the date at midnight UTC. ok is false when the
// series has no bar for exactly that session (a gap, or a listing whose
// sessions are not NYSE's), so the caller never measures from a different
// session's close.
func Baseline(bars []DailyBar, observedAt time.Time) (refClose, atr float64, ok bool) {
	session := CompletedThrough(observedAt)
	for session.Weekday() == time.Saturday || session.Weekday() == time.Sunday || IsUSMarketHoliday(session) {
		session = session.AddDate(0, 0, -1)
	}
	upTo := make([]DailyBar, 0, len(bars))
	for _, b := range bars {
		if !b.Session.After(session) {
			upTo = append(upTo, b)
		}
	}
	if len(upTo) == 0 {
		return 0, 0, false
	}
	sort.Slice(upTo, func(i, j int) bool { return upTo[i].Session.Before(upTo[j].Session) })
	last := upTo[len(upTo)-1]
	if !last.Session.Equal(session) || last.Close <= 0 {
		return 0, 0, false
	}
	return last.Close, wilderATR(upTo, 14), true
}

// wilderATR is the Wilder ATR of the last bar, the scorecard's definition
// (services/broker-ta indicators.atr): the mean of the first period true
// ranges after bar 0, then smoothed with alpha 1/period. 0 when there are
// too few bars or a bar has no high/low.
func wilderATR(bars []DailyBar, period int) float64 {
	if len(bars) <= period {
		return 0
	}
	trueRange := func(i int) float64 {
		b, prev := bars[i], bars[i-1].Close
		return math.Max(b.High-b.Low, math.Max(math.Abs(b.High-prev), math.Abs(b.Low-prev)))
	}
	for _, b := range bars {
		if b.High <= 0 || b.Low <= 0 {
			return 0
		}
	}
	atr := 0.0
	for i := 1; i <= period; i++ {
		atr += trueRange(i)
	}
	atr /= float64(period)
	for i := period + 1; i < len(bars); i++ {
		atr = (atr*float64(period-1) + trueRange(i)) / float64(period)
	}
	return atr
}

// ForwardOutcome measures a candidate observed in session observed, at
// reference close refClose and ATR refATR, over the next h completed
// sessions. Sessions are the bars' own dates, so weekends and holidays
// never count. A bar is completed when its session is on or before
// completedThrough. ok is false until h completed sessions exist or when
// refClose is not positive.
func ForwardOutcome(bars []DailyBar, observed time.Time, refClose, refATR float64, h int, completedThrough time.Time) (Outcome, bool) {
	if refClose <= 0 || h <= 0 {
		return Outcome{}, false
	}
	after := make([]DailyBar, 0, h)
	for _, b := range bars {
		if b.Session.After(observed) && !b.Session.After(completedThrough) {
			after = append(after, b)
		}
	}
	sort.Slice(after, func(i, j int) bool { return after[i].Session.Before(after[j].Session) })
	if len(after) < h {
		return Outcome{}, false
	}
	window := after[:h]
	hi, lo := window[0].High, window[0].Low
	for _, b := range window[1:] {
		hi = math.Max(hi, b.High)
		lo = math.Min(lo, b.Low)
	}
	o := Outcome{Return: window[h-1].Close/refClose - 1, MFE: math.NaN(), MAE: math.NaN()}
	if refATR > 0 {
		o.MFE = (hi - refClose) / refATR
		o.MAE = (lo - refClose) / refATR
	}
	return o, true
}

// SessionDate is the New York calendar date of an instant, at midnight UTC,
// the form DailyBar.Session uses.
func SessionDate(at time.Time) time.Time {
	ny := at
	if tz, err := time.LoadLocation("America/New_York"); err == nil {
		ny = at.In(tz)
	}
	return time.Date(ny.Year(), ny.Month(), ny.Day(), 0, 0, 0, 0, time.UTC)
}

// CompletedThrough is the latest session date whose daily bar is final at
// now: today once the 16:00 ET close has passed, else yesterday. Sessions
// are the bars' own dates, so a weekend or holiday never needs a rule.
func CompletedThrough(now time.Time) time.Time {
	today := SessionDate(now)
	if tz, err := time.LoadLocation("America/New_York"); err == nil {
		if ny := now.In(tz); ny.Hour() >= 16 {
			return today
		}
	}
	return today.AddDate(0, 0, -1)
}

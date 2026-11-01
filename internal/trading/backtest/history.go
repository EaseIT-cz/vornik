// Package backtest replays the ibkr-trader v2.1 rules over past daily
// history (GitHub #599). It reads the file broker-ta's history module
// writes and decides each session with the unchanged rules.Decide. It is
// offline measurement: no database, no broker, no trade.
// Design: https://docs.vornik.io
package backtest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"vornik.io/vornik/internal/trading/rules"
)

// Bar is one session's prices.
type Bar struct{ Open, High, Low, Close float64 }

// History is the replayed record, keyed by session (YYYY-MM-DD). A
// session's scorecards and regimes are what the live tick saw before that
// session opened.
type History struct {
	Sessions []string
	Regions  map[string]string // universe symbol -> us, eu or apac
	Bars     map[string]map[string]Bar
	Scores   map[string]map[string]rules.Scorecard
	Regimes  map[string]map[string]rules.Regime
	// Earnings holds each universe symbol's report dates; a present nil
	// means the source had none (not modelled).
	Earnings map[string][]string
}

type line struct {
	Kind       string            `json:"kind"`
	Session    string            `json:"session"`
	Symbol     string            `json:"symbol"`
	Region     string            `json:"region"`
	Symbols    map[string]string `json:"symbols"`
	Dates      []string          `json:"dates"`
	Label      string            `json:"label"`
	Score      int               `json:"score"`
	Components int               `json:"components"`
	Stale      bool              `json:"stale"`
	Open       float64           `json:"open"`
	High       float64           `json:"high"`
	Low        float64           `json:"low"`
	Close      float64           `json:"close"`
	Total      int               `json:"total"`
	Trend      int               `json:"trend"`
	Momentum   int               `json:"momentum"`
	Macro      int               `json:"macro"`
	LastClose  *float64          `json:"last_close"`
	SMA20      *float64          `json:"sma20"`
	SMA50      *float64          `json:"sma50"`
	SMA200     *float64          `json:"sma200"`
	RSI14      *float64          `json:"rsi14"`
	ATR14      *float64          `json:"atr14"`
}

// Load reads a history file. A scorecard with a null value is dropped: the
// engine treats a missing scorecard as insufficient_inputs.
func Load(r io.Reader) (*History, error) {
	h := &History{Regions: map[string]string{}, Bars: map[string]map[string]Bar{},
		Scores: map[string]map[string]rules.Scorecard{}, Regimes: map[string]map[string]rules.Regime{},
		Earnings: map[string][]string{}}
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("history line %d: %w", n, err)
		}
		if l.Session != "" {
			seen[l.Session] = true
		}
		switch l.Kind {
		case "meta":
			for s, r := range l.Symbols {
				h.Regions[s] = r
			}
		case "earnings":
			h.Earnings[l.Symbol] = l.Dates
		case "regime":
			at(h.Regimes, l.Session)[l.Region] = rules.Regime{Label: l.Label, Score: l.Score, Stale: l.Stale, Components: l.Components}
		case "bar":
			at(h.Bars, l.Session)[l.Symbol] = Bar{Open: l.Open, High: l.High, Low: l.Low, Close: l.Close}
		case "scorecard":
			if s, ok := scorecard(l); ok {
				at(h.Scores, l.Session)[l.Symbol] = s
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for s := range seen {
		h.Sessions = append(h.Sessions, s)
	}
	sort.Strings(h.Sessions)
	return h, nil
}

func at[V any](m map[string]map[string]V, k string) map[string]V {
	if m[k] == nil {
		m[k] = map[string]V{}
	}
	return m[k]
}

func scorecard(l line) (rules.Scorecard, bool) {
	for _, v := range []*float64{l.LastClose, l.SMA20, l.SMA50, l.SMA200, l.RSI14, l.ATR14} {
		if v == nil {
			return rules.Scorecard{}, false
		}
	}
	return rules.Scorecard{Total: l.Total, Trend: l.Trend, Momentum: l.Momentum, Macro: l.Macro,
		LastClose: *l.LastClose, SMA20: *l.SMA20, SMA50: *l.SMA50, SMA200: *l.SMA200,
		RSI14: *l.RSI14, ATR14: *l.ATR14}, true
}

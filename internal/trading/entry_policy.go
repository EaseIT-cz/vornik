package trading

import (
	"fmt"
	"math"
)

// EntryPolicy limits proposed entries independently of the scorecard switches.
// AllowedSymbols is an entry universe, not a restriction on closing holdings.
// Dollar risk is planned loss at the submitted stop, not a guaranteed loss cap:
// gaps, slippage and commissions can increase the actual loss.
type EntryPolicy struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	AllowedSymbols      []string `yaml:"allowed_symbols" json:"allowed_symbols"`
	LongOnly            bool     `yaml:"long_only" json:"long_only"`
	NoPositionAdditions bool     `yaml:"no_position_additions" json:"no_position_additions"`
	MaxRiskUSD          float64  `yaml:"max_risk_usd" json:"max_risk_usd"`
	MinNotionalUSD      float64  `yaml:"min_notional_usd" json:"min_notional_usd"`
	MaxEntriesPerTick   int      `yaml:"max_entries_per_tick" json:"max_entries_per_tick"`
	// Portfolio gates (2026-09-10), broker-only because the workflow filter
	// never sees holdings. Zero is off. Gross exposure counts held market
	// value plus working non-child order notional plus the order at hand;
	// positions count distinct held plus pending symbols; the daily-loss
	// pause refuses opens once the session's realised P&L is at or below
	// the negated value.
	MaxGrossExposureUSD float64 `yaml:"max_gross_exposure_usd" json:"max_gross_exposure_usd"`
	MaxPositions        int     `yaml:"max_positions" json:"max_positions"`
	DailyLossPauseUSD   float64 `yaml:"daily_loss_pause_usd" json:"daily_loss_pause_usd"`
	// PullbackAdditions allows ONE bounded addition to a winning long on a
	// pullback (amendment 2026-09-25). Mutually exclusive with
	// NoPositionAdditions: an enabled policy answers "additions" exactly once.
	PullbackAdditions PullbackAdditions `yaml:"pullback_additions" json:"pullback_additions"`
}

// PullbackAdditions is the conditional-addition rule. Only the pullback shape
// is configurable; one add per position, no larger than the original entry,
// is the operator's decision and lives in code.
type PullbackAdditions struct {
	Enabled        bool    `yaml:"enabled" json:"enabled" since:"2026.9.7"`
	MaxDistanceATR float64 `yaml:"max_distance_atr" json:"max_distance_atr" since:"2026.9.7"`
	RSIMin         float64 `yaml:"rsi_min" json:"rsi_min" since:"2026.9.7"`
	RSIMax         float64 `yaml:"rsi_max" json:"rsi_max" since:"2026.9.7"`
}

// AddEvidence is the canonical `add_evidence` block an `intent: add`
// proposal carries: the position's broker average cost and the symbol's
// scorecard values, each proven equal to a tool output by the
// analysis-evidence gate.
type AddEvidence struct {
	AvgCost   float64 `json:"avg_cost"`
	LastClose float64 `json:"last_close"`
	SMA20     float64 `json:"sma20"`
	SMA50     float64 `json:"sma50"`
	SMA200    float64 `json:"sma200"`
	RSI14     float64 `json:"rsi14"`
	ATR14     float64 `json:"atr14"`
}

func (e AddEvidence) complete() bool {
	for _, v := range []float64{e.AvgCost, e.LastClose, e.SMA20, e.SMA50, e.SMA200, e.RSI14, e.ATR14} {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Reason is the rule's single implementation: "" when an add at limit
// qualifies, else the rejection reason. In order: evidence complete; a winner
// (avg cost below last close); never below cost (limit >= avg cost); trend
// intact (last close > sma50 > sma200, sma20 > sma50); a pullback
// (|last close - sma20| <= distance x atr14); RSI in band; and the limit
// itself inside sma20 ± distance x atr14, so it cannot fill as a chase.
func (p PullbackAdditions) Reason(e AddEvidence, limit float64) string {
	band := p.MaxDistanceATR * e.ATR14
	switch {
	case !e.complete() || limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0):
		return "add_evidence_missing"
	case !(e.AvgCost < e.LastClose):
		return "add_not_in_profit"
	case limit < e.AvgCost:
		return "add_below_cost"
	case !(e.LastClose > e.SMA50 && e.SMA50 > e.SMA200 && e.SMA20 > e.SMA50):
		return "add_trend_broken"
	case math.Abs(e.LastClose-e.SMA20) > band:
		return "add_not_pullback"
	case e.RSI14 < p.RSIMin || e.RSI14 > p.RSIMax:
		return "add_rsi_out_of_band"
	case limit > e.SMA20+band || limit < e.SMA20-band:
		return "add_limit_outside_band"
	}
	return ""
}

func (p PullbackAdditions) validate() error {
	if !p.Enabled {
		return nil
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(p.MaxDistanceATR) || p.MaxDistanceATR <= 0 {
		return fmt.Errorf("pullback_additions.max_distance_atr must be a positive finite number")
	}
	if !finite(p.RSIMin) || !finite(p.RSIMax) || p.RSIMin < 0 || p.RSIMax > 100 || p.RSIMin >= p.RSIMax {
		return fmt.Errorf("pullback_additions rsi band must be finite, within [0, 100], with rsi_min < rsi_max")
	}
	return nil
}

// Validate rejects settings that could silently disable a numeric gate.
func (p EntryPolicy) Validate() error {
	for _, n := range []float64{p.MaxRiskUSD, p.MinNotionalUSD, p.MaxGrossExposureUSD, p.DailyLossPauseUSD} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return fmt.Errorf("entry risk, notional, exposure and loss-pause limits must be finite and nonnegative")
		}
	}
	if p.MaxEntriesPerTick < 0 || p.MaxPositions < 0 {
		return fmt.Errorf("max_entries_per_tick and max_positions cannot be negative")
	}
	if p.Enabled && (p.MaxRiskUSD == 0 || p.MaxEntriesPerTick == 0) {
		return fmt.Errorf("enabled entry policy requires positive max_risk_usd and max_entries_per_tick")
	}
	if err := p.PullbackAdditions.validate(); err != nil {
		return err
	}
	// Exactly two answers to "may a held position be added to" (amendment
	// 2026-09-25, review F1): no (no_position_additions) or conditionally
	// (pullback_additions). Neither would allow unconditional adds — the v1
	// behaviour the 2026-09-09 audit banned; both is a contradiction.
	if p.Enabled && p.NoPositionAdditions == p.PullbackAdditions.Enabled {
		return fmt.Errorf("enabled entry policy must set exactly one of no_position_additions and pullback_additions.enabled")
	}
	return nil
}

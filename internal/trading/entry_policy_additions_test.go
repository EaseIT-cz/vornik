package trading

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// Amendment 2026-09-25 (trading-entry-policy design): one bounded pullback
// addition to a winner. An enabled policy has exactly two valid states for
// additions — OFF (no_position_additions) or CONDITIONAL (pullback_additions);
// `no_position_additions: false` with pullback off would re-open the
// unconditional adds the 2026-09-09 audit banned (design review F1).

func addPolicy() EntryPolicy {
	return EntryPolicy{Enabled: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1,
		PullbackAdditions: PullbackAdditions{Enabled: true, MaxDistanceATR: 1, RSIMin: 45, RSIMax: 60}}
}

func TestEntryPolicyAdditionStates(t *testing.T) {
	off := EntryPolicy{Enabled: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1, NoPositionAdditions: true}
	require.NoError(t, off.Validate(), "additions off")
	require.NoError(t, addPolicy().Validate(), "additions conditional")

	both := addPolicy()
	both.NoPositionAdditions = true
	require.Error(t, both.Validate(), "two answers to one question")

	unconditional := EntryPolicy{Enabled: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1}
	require.Error(t, unconditional.Validate(), "no_position_additions false with pullback off re-opens unconditional adds")

	// A disabled policy imposes nothing, so it needs no addition answer.
	require.NoError(t, EntryPolicy{}.Validate())
}

func TestPullbackAdditionThresholds(t *testing.T) {
	for name, mutate := range map[string]func(p *PullbackAdditions){
		"zero distance":     func(p *PullbackAdditions) { p.MaxDistanceATR = 0 },
		"nan distance":      func(p *PullbackAdditions) { p.MaxDistanceATR = math.NaN() },
		"inf distance":      func(p *PullbackAdditions) { p.MaxDistanceATR = math.Inf(1) },
		"nan rsi min":       func(p *PullbackAdditions) { p.RSIMin = math.NaN() },
		"nan rsi max":       func(p *PullbackAdditions) { p.RSIMax = math.NaN() },
		"band above 100":    func(p *PullbackAdditions) { p.RSIMax = 101 },
		"band below 0":      func(p *PullbackAdditions) { p.RSIMin = -1 },
		"min not below max": func(p *PullbackAdditions) { p.RSIMin, p.RSIMax = 60, 60 },
	} {
		p := addPolicy()
		mutate(&p.PullbackAdditions)
		require.Error(t, p.Validate(), name)
	}
}

// The conditions, one function, shared by the filter so the rule has a single
// implementation.
func TestPullbackAdditionConditions(t *testing.T) {
	pa := addPolicy().PullbackAdditions
	good := AddEvidence{AvgCost: 90, LastClose: 95, SMA20: 94, SMA50: 91, SMA200: 83, RSI14: 52, ATR14: 1.6}
	require.Equal(t, "", pa.Reason(good, 95.1))
	for want, tc := range map[string]struct {
		ev    func(e *AddEvidence)
		limit float64
	}{
		"add_not_in_profit":      {func(e *AddEvidence) { e.AvgCost = 95 }, 95.1},
		"add_below_cost":         {func(e *AddEvidence) { e.AvgCost = 94.5 }, 94.4},
		"add_trend_broken":       {func(e *AddEvidence) { e.SMA50 = 84; e.SMA200 = 85 }, 95.1},
		"add_not_pullback":       {func(e *AddEvidence) { e.LastClose, e.SMA20 = 97, 94 }, 95.1},
		"add_rsi_out_of_band":    {func(e *AddEvidence) { e.RSI14 = 66 }, 95.1},
		"add_limit_outside_band": {func(*AddEvidence) {}, 95.7},
		"add_evidence_missing":   {func(e *AddEvidence) { e.ATR14 = 0 }, 95.1},
	} {
		ev := good
		tc.ev(&ev)
		require.Equal(t, want, pa.Reason(ev, tc.limit), want)
	}
	// The limit band is two-sided (design round 2 F1): a deep resting limit
	// above cost is not a pullback at today's price.
	require.Equal(t, "add_limit_outside_band", pa.Reason(good, 92.3))
}

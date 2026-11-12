package executor

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #719 (found at production, 2026-11-10: HDB ret_5 +17.7% with the
// lowest low 3 ATR above the reference; most rows with NULL ref_atr). The
// stored ref_close is the scorecard's (yfinance) last_close; the forward bars
// are the broker's. The baseline must be the broker series' close of the
// session last_close describes, Mon 11-09 for a Tue 11:00 ET tick, and the
// ATR the broker series' ATR14 through it.
//
// The three candidate baselines give three distinct answers for ret_1:
//   - stored ref_close 100 (the bug, mixed sources): 102/100-1 = +2.0%;
//   - the observed session's own close 105 (set after the tick): -2.9%;
//   - the broker close of 11-09, 101 (the fix): 102/101-1 = +0.99%.
//
// ref_atr is NULL, as on most production rows: before the fix MFE/MAE stayed
// NULL; now they use the bars' ATR14 (every true range 2, so ATR 2).
func TestFunnelOutcomes_BaselineFromBarSeries_719(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	repo := &outcomeRepo{pending: []persistence.FunnelCandidate{
		{ExecutionID: "e1", Symbol: "HDB", Intent: "open", ObservedAt: ny(11, 10, 11), RefClose: f(100)},
	}}
	e := &Executor{logger: zerolog.Nop(), tradingFunnelRepo: repo}
	bars := flatSessionsThrough(ny(11, 9, 0), 15) // H 102, L 100, C 101
	bars = append(bars,
		barClose{TS: ny(11, 10, 0), High: 106, Low: 101, Close: 105},
		barClose{TS: ny(11, 11, 0), High: 104, Low: 100, Close: 102},
	)
	e.fillFunnelOutcomes(context.Background(), "trade", "HDB", bars, ny(11, 11, 17))

	require.Contains(t, repo.set, "e1/0")
	got := repo.set["e1/0"]
	assert.InDelta(t, 102.0/101-1, got[0], 1e-9, "ret_1 from the broker close of 11-09")
	assert.InDelta(t, (104.0-101)/2, got[1], 1e-9, "mfe_1 from the same baseline and the bars' ATR")
	assert.InDelta(t, (100.0-101)/2, got[2], 1e-9, "mae_1 from the same baseline and the bars' ATR")
}

// Without the baseline session in the fetched bars, the stored ref_close and
// ref_atr are used together (the pre-#719 behaviour); the outcome is still
// written.
func TestFunnelOutcomes_BaselineFallsBackToStoredPair_719(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	repo := &outcomeRepo{pending: []persistence.FunnelCandidate{
		{ExecutionID: "e1", Symbol: "HDB", Intent: "open", ObservedAt: ny(11, 10, 11), RefClose: f(100), RefATR: f(2)},
	}}
	e := &Executor{logger: zerolog.Nop(), tradingFunnelRepo: repo}
	bars := []barClose{
		{TS: ny(11, 10, 0), High: 106, Low: 101, Close: 105},
		{TS: ny(11, 11, 0), High: 104, Low: 100, Close: 102},
	}
	e.fillFunnelOutcomes(context.Background(), "trade", "HDB", bars, ny(11, 11, 17))

	require.Contains(t, repo.set, "e1/0")
	got := repo.set["e1/0"]
	assert.InDelta(t, 0.02, got[0], 1e-9)
	assert.InDelta(t, (104.0-100)/2, got[1], 1e-9, "mfe_1 from the stored pair")
}

// With the baseline bar present but only 14 bars up to it, the bars give no
// ATR: the return is written and MFE/MAE stay NULL. The stored ref_atr is
// not borrowed, so a bar close is never paired with a yfinance ATR.
func TestFunnelOutcomes_UnknownBarATRLeavesMFEMAENull_719(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	repo := &outcomeRepo{pending: []persistence.FunnelCandidate{
		{ExecutionID: "e1", Symbol: "HDB", Intent: "open", ObservedAt: ny(11, 10, 11), RefClose: f(100), RefATR: f(2)},
	}}
	e := &Executor{logger: zerolog.Nop(), tradingFunnelRepo: repo}
	bars := append(flatSessionsThrough(ny(11, 9, 0), 14),
		barClose{TS: ny(11, 10, 0), High: 106, Low: 101, Close: 105},
		barClose{TS: ny(11, 11, 0), High: 104, Low: 100, Close: 102},
	)
	e.fillFunnelOutcomes(context.Background(), "trade", "HDB", bars, ny(11, 11, 17))

	require.Contains(t, repo.set, "e1/0")
	got := repo.set["e1/0"]
	assert.InDelta(t, 102.0/101-1, got[0], 1e-9, "ret_1 from the bar close")
	assert.Equal(t, [2]float64{-999, -999}, [2]float64{got[1], got[2]}, "no bar ATR: MFE/MAE NULL")
}

// flatSessionsThrough returns n weekday bars ending on last, each with high
// 102, low 100 and close 101: every true range is 2.
func flatSessionsThrough(last time.Time, n int) []barClose {
	out := make([]barClose, n)
	d := last
	for i := n - 1; i >= 0; i-- {
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, -1)
		}
		out[i] = barClose{TS: d, High: 102, Low: 100, Close: 101}
		d = d.AddDate(0, 0, -1)
	}
	return out
}

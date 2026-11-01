package executor

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/trading"
)

// The entry funnel, recorded by the daemon (GitHub #596; trading
// entry-policy design, Amendment 2026-10-31). The strategist's accepted
// output carries one entry_funnel row per eligible symbol; this records
// them with the gate name normalised and the tick's reference close and
// ATR, so each candidate can be scored on its own later. Measurement only:
// nothing here can fail a step or change a trade.

// funnelRowsTotal is nil until RegisterFunnelMetrics wires it; increments
// are nil-guarded.
var funnelRowsTotal *prometheus.CounterVec

// RegisterFunnelMetrics registers vornik_trading_funnel_rows_total.
func RegisterFunnelMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	funnelRowsTotal = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "trading",
		Name:      "funnel_rows_total",
		Help: "Strategist entry-funnel rows seen by the daemon, by result: recorded, skipped_malformed " +
			"(no parseable funnel in the accepted output) or write_failed. Measurement only.",
	}, []string{"result"})
}

func incFunnelRows(result string, n int) {
	if funnelRowsTotal != nil && n > 0 {
		funnelRowsTotal.WithLabelValues(result).Add(float64(n))
	}
}

// recordFunnel stores the accepted output's entry funnel. Never returns an
// error: a failure is counted and logged.
func (e *Executor) recordFunnel(ctx context.Context, in *StepOutcome) {
	if e.tradingFunnelRepo == nil || in == nil || in.Task == nil || in.Execution == nil {
		return
	}
	rows, ok := trading.ParseFunnel(in.ResultBytes)
	if !ok {
		incFunnelRows("skipped_malformed", 1)
		return
	}
	now := evidenceNow().UTC()
	out := make([]persistence.FunnelCandidate, 0, len(rows))
	for _, r := range rows {
		c := persistence.FunnelCandidate{
			ExecutionID: in.Execution.ID, Symbol: r.Symbol, Intent: r.Intent,
			TaskID: in.Task.ID, ProjectID: in.Task.ProjectID, StepID: in.StepID,
			ObservedAt: now, Status: r.Status, Gate: trading.NormalizeGate(r.Gate),
			GateRaw: r.Gate, Reason: r.Reason, Inputs: r.Inputs,
		}
		if refClose, refATR := trading.RefPrices(r.Inputs); refClose > 0 {
			c.RefClose = &refClose
			if refATR > 0 {
				c.RefATR = &refATR
			}
		}
		out = append(out, c)
	}
	if err := e.tradingFunnelRepo.Record(ctx, out); err != nil {
		incFunnelRows("write_failed", len(out))
		e.logger.Warn().Err(err).Str("task_id", in.Task.ID).Str("step", in.StepID).
			Msg("trading entry funnel not recorded (measurement only; the step is unaffected)")
		return
	}
	incFunnelRows("recorded", len(out))
}

// fillFunnelOutcomes scores the pending funnel candidates of one symbol
// from the daily bars the indicator pre-warm just fetched. Each horizon's
// outcome is written once, when its sessions are final at now. Errors are
// logged, never returned.
func (e *Executor) fillFunnelOutcomes(ctx context.Context, projectID, symbol string, bars []barClose, now time.Time) {
	if e.tradingFunnelRepo == nil || len(bars) == 0 {
		return
	}
	pending, err := e.tradingFunnelRepo.Pending(ctx, projectID, symbol)
	if err != nil || len(pending) == 0 {
		return
	}
	daily := make([]trading.DailyBar, 0, len(bars))
	for _, b := range bars {
		daily = append(daily, trading.DailyBar{Session: trading.SessionDate(b.TS), High: b.High, Low: b.Low, Close: b.Close})
	}
	through := trading.CompletedThrough(now)
	for _, c := range pending {
		if c.RefClose == nil {
			continue
		}
		atr := 0.0
		if c.RefATR != nil {
			atr = *c.RefATR
		}
		observed := trading.SessionDate(c.ObservedAt)
		for i, h := range persistence.FunnelHorizons {
			if c.Ret[i] != nil {
				continue
			}
			o, ok := trading.ForwardOutcome(daily, observed, *c.RefClose, atr, h, through)
			if !ok {
				continue
			}
			if err := e.tradingFunnelRepo.SetOutcome(ctx, c.ExecutionID, c.Symbol, c.Intent, i, o.Return, finite(o.MFE), finite(o.MAE)); err != nil {
				e.logger.Warn().Err(err).Str("symbol", symbol).Msg("trading funnel outcome not recorded (measurement only)")
				return
			}
		}
	}
}

// finite returns a pointer to v, or nil for NaN: an unknown ATR measure is
// stored as NULL.
func finite(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// recordBenchmarkCloses records a benchmark symbol's completed-session
// closes from the bars the pre-warm just fetched (GitHub #597). The
// in-progress session is skipped; a repeat is a no-op in the store. Errors
// are logged, never returned.
func (e *Executor) recordBenchmarkCloses(ctx context.Context, benchmarks []string, symbol string, bars []barClose, now time.Time) {
	if e.tradingBenchmarkRepo == nil || !slices.Contains(benchmarks, symbol) {
		return
	}
	through := trading.CompletedThrough(now)
	closes := make([]persistence.BenchmarkClose, 0, len(bars))
	for _, b := range bars {
		if s := trading.SessionDate(b.TS); !s.After(through) && b.Close > 0 {
			closes = append(closes, persistence.BenchmarkClose{Symbol: symbol, Session: s, Close: b.Close})
		}
	}
	if err := e.tradingBenchmarkRepo.Record(ctx, closes); err != nil {
		e.logger.Warn().Err(err).Str("symbol", symbol).Msg("benchmark closes not recorded (measurement only)")
	}
}

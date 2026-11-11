package executor

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
	"vornik.io/vornik/internal/trading/rules"
)

// The rule engine in shadow (GitHub #598 step 1; trading entry-policy
// design, Amendment 2026-10-31 §2). It decides the tick from the
// strategist step's own tool calls and records its decision beside the
// model's. Measurement only: it never fails a step or changes a trade.

// shadowRowsTotal is nil until RegisterShadowMetrics wires it.
var shadowRowsTotal *prometheus.CounterVec

// RegisterShadowMetrics registers vornik_trading_shadow_rows_total.
func RegisterShadowMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	shadowRowsTotal = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "trading",
		Name:      "shadow_rows_total",
		Help: "Rule-engine shadow decisions recorded beside the strategist's, by result: agree, disagree " +
			"or write_failed. Measurement only.",
	}, []string{"result"})
}

func incShadow(result string, n int) {
	if shadowRowsTotal != nil && n > 0 {
		shadowRowsTotal.WithLabelValues(result).Add(float64(n))
	}
}

// recordShadow decides the tick with the rule engine and records it beside
// the model's funnel. Never returns an error.
func (e *Executor) recordShadow(ctx context.Context, in *StepOutcome, calls []trading.ToolCall, p *registry.Project) {
	if e.tradingShadowRepo == nil || in == nil || in.Task == nil || in.Execution == nil || p == nil {
		return
	}
	now := evidenceNow().UTC()
	ins := trading.ShadowInputs(calls, p.Trading.EntryPolicy.AllowedSymbols, now, 0.01)
	pol := enginePolicy(p)
	e.addOrderState(ctx, &ins, p.ID, now, pol)
	d := rules.Decide(ins, pol, engineCaps(p))
	e.writeShadow(ctx, in, d.Funnel, in.ResultBytes)
}

// writeShadow records the engine's rows beside the model's funnel, read from
// modelBytes. In engine mode the rows are the decision that ran (#717).
func (e *Executor) writeShadow(ctx context.Context, in *StepOutcome, engine []rules.FunnelRow, modelBytes []byte) {
	if e.tradingShadowRepo == nil || in == nil || in.Task == nil || in.Execution == nil {
		return
	}
	now := evidenceNow().UTC()
	model := map[string][2]string{}
	if rows, ok := trading.ParseFunnel(modelBytes); ok {
		for _, r := range rows {
			model[strings.ToUpper(r.Symbol)+"|"+r.Intent] = [2]string{r.Status, trading.NormalizeGate(r.Gate)}
		}
	}
	out := make([]persistence.ShadowDecision, 0, len(engine))
	var agree int
	for _, r := range engine {
		m, ok := model[r.Symbol+"|"+r.Intent]
		row := persistence.ShadowDecision{ExecutionID: in.Execution.ID, Symbol: r.Symbol, Intent: r.Intent,
			ProjectID: in.Task.ProjectID, ObservedAt: now, EngineStatus: r.Status, EngineGate: r.Gate,
			ModelStatus: m[0], ModelGate: m[1], Agree: ok && m[0] == r.Status}
		if row.Agree {
			agree++
		}
		out = append(out, row)
	}
	if err := e.tradingShadowRepo.Record(ctx, out); err != nil {
		incShadow("write_failed", len(out))
		e.logger.Warn().Err(err).Str("task_id", in.Task.ID).Msg("rule-engine shadow not recorded (measurement only)")
		return
	}
	incShadow("agree", agree)
	incShadow("disagree", len(out)-agree)
}

// enginePolicy is the rule engine's policy for a project: v2.1's defaults
// with the floor's own settings, so the floor is never stricter than the
// engine: the regime threshold (GitHub #716), the minimum total and the
// regime panel counts (GitHub #717).
func enginePolicy(p *registry.Project) rules.Policy {
	pol := rules.DefaultPolicy()
	pol.BlockLongAtScore = p.Trading.Regime.LongBlockScore()
	if n := p.Trading.Scorecard.MinEntryTotal; n > 0 {
		pol.MinTotal = n
	}
	if len(p.Trading.Regime.MinComponentCount) > 0 {
		pol.RegimePanel = p.Trading.Regime.MinComponentCount
	}
	return pol
}

// addOrderState fills the cooldown and today's entry count from the daemon's
// own order records. A read failure leaves them empty.
func (e *Executor) addOrderState(ctx context.Context, in *rules.Inputs, projectID string, now time.Time, pol rules.Policy) {
	if e.tradingOrderRepo == nil {
		return
	}
	since := now.AddDate(0, 0, -10)
	pid := projectID
	orders, err := e.tradingOrderRepo.List(ctx, persistence.TradingOrderFilter{ProjectID: &pid, Since: &since, PageSize: 500})
	if err != nil {
		return
	}
	today := trading.SessionDate(now)
	through := trading.CompletedThrough(now)
	for _, o := range orders {
		if o == nil {
			continue
		}
		sym := strings.ToUpper(o.Symbol)
		switch strings.ToUpper(o.Action) {
		case "SELL":
			at := o.SubmittedAt
			if o.TerminalAt != nil {
				at = *o.TerminalAt
			}
			if o.FilledQty > 0 && completedSessionsAfter(trading.SessionDate(at), through) < pol.CooldownSessions {
				in.CooledDown[sym] = true
			}
		case "BUY":
			child := strings.HasSuffix(o.IdempotencyKey, "_stop") || strings.HasSuffix(o.IdempotencyKey, "_tp")
			dead := strings.EqualFold(o.Status, "rejected") || strings.EqualFold(o.Status, "cancelled") || strings.EqualFold(o.Status, "canceled")
			if !child && trading.SessionDate(o.SubmittedAt).Equal(today) && (o.FilledQty > 0 || !dead) {
				in.EntriesToday++
			}
		}
	}
}

// completedSessionsAfter counts US sessions after day up to through.
func completedSessionsAfter(day, through time.Time) int {
	n := 0
	for d := day.AddDate(0, 0, 1); !d.After(through); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday && !trading.IsUSMarketHoliday(d) {
			n++
		}
	}
	return n
}

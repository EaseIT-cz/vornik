package narrator

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the narrator's Prometheus counters (design §5.9).
// Nil-safe throughout — a nil *Metrics disables emission, mirroring
// memory/metrics.go + livepubsub/metrics.go's idiom.
type Metrics struct {
	// LinesTotal — narration lines produced, by storage kind
	// (step|tool|milestone|completion) and whether the line was a
	// deterministic-fallback ("degraded").
	LinesTotal *prometheus.CounterVec
	// BudgetCappedTotal — executions that hit the line cap or cost
	// budget, by reason (lines|cost).
	BudgetCappedTotal *prometheus.CounterVec
	// DroppedEventsTotal — bus events the narrator dropped under
	// load or on an unresolvable execution, by kind.
	DroppedEventsTotal *prometheus.CounterVec
	// PanicsTotal — recovered Run-loop panics (design §5.1).
	PanicsTotal prometheus.Counter
	// DegradedTotal — narration lines that fell back to the deterministic
	// template, by cause (outcome-quality LLD §S8.11 row 11, GitHub #369):
	// no_client, llm_timeout, llm_cancelled, llm_error, no_choices, empty,
	// cost_capped, empty_text. lines_total{degraded="true"} is the sum.
	DegradedTotal *prometheus.CounterVec
}

// NewMetrics creates and registers the narrator metrics. Returns nil
// when reg is nil (observability disabled), matching
// livepubsub.NewMetrics's contract exactly.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		LinesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "narration",
			Name:      "lines_total",
			Help:      "Narration lines produced by the narrator worker, by storage kind and whether the line was a deterministic fallback.",
		}, []string{"kind", "degraded"}),
		BudgetCappedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "narration",
			Name:      "budget_capped_total",
			Help:      "Executions where narration hit the per-execution line cap or cost budget, by reason.",
		}, []string{"reason"}),
		DroppedEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "narration",
			Name:      "dropped_events_total",
			Help:      "Bus events and lines the narrator dropped, by kind: min_line_interval, persist_error, line_capped, chatpush_lookup_error, chatpush_task_missing, chatpush_send_error.",
		}, []string{"kind"}),
		PanicsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "narration",
			Name:      "panics_total",
			Help:      "Recovered panics in the narrator's Run loop. The daemon is never affected; the loop re-subscribes.",
		}),
		DegradedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "narration",
			Name:      "degraded_total",
			Help:      "Narration lines that fell back to the deterministic template, by cause: no_client, llm_timeout, llm_cancelled, llm_error, no_choices, empty, cost_capped, empty_text.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.LinesTotal, m.BudgetCappedTotal, m.DroppedEventsTotal, m.PanicsTotal, m.DegradedTotal)
	return m
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (n *Narrator) metricLine(kind string, degraded bool) {
	if n == nil || n.Metrics == nil || n.Metrics.LinesTotal == nil {
		return
	}
	n.Metrics.LinesTotal.WithLabelValues(kind, boolLabel(degraded)).Inc()
}

func (n *Narrator) metricCapped(reason string) {
	if n == nil || n.Metrics == nil || n.Metrics.BudgetCappedTotal == nil {
		return
	}
	n.Metrics.BudgetCappedTotal.WithLabelValues(reason).Inc()
}

func (n *Narrator) metricDropped(kind string) {
	if n == nil || n.Metrics == nil || n.Metrics.DroppedEventsTotal == nil {
		return
	}
	n.Metrics.DroppedEventsTotal.WithLabelValues(kind).Inc()
}

func (n *Narrator) metricDegraded(reason string) {
	if n == nil || n.Metrics == nil || n.Metrics.DegradedTotal == nil {
		return
	}
	n.Metrics.DegradedTotal.WithLabelValues(reason).Inc()
}

func (n *Narrator) metricPanic() {
	if n == nil || n.Metrics == nil || n.Metrics.PanicsTotal == nil {
		return
	}
	n.Metrics.PanicsTotal.Inc()
}

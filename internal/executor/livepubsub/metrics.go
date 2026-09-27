package livepubsub

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the live-event publisher
// (live-task-observation LLD §10). published is counted at the local
// publish seam; dropped is counted when a non-blocking fan-out skips a
// slow subscriber. Both are nil-safe — a nil *Metrics disables emission.
type Metrics struct {
	PublishedTotal *prometheus.CounterVec // {kind}
	DroppedTotal   *prometheus.CounterVec // {reason}
	// DetachedTimeoutTotal counts a DB-backed publish phase that ran out its
	// own bound after being detached from the caller's cancellation
	// (amendment 2026-09-25). phase=append: the event is lost to other
	// replicas; phase=notify: delayed until ListSince catch-up, not lost.
	DetachedTimeoutTotal *prometheus.CounterVec // {phase}
}

// NewMetrics creates and registers the live-event metrics. Returns nil
// when reg is nil (observability disabled) — callers and the publisher
// nil-check, so this stays a no-op rather than registering on a phantom
// registry. Takes the concrete *prometheus.Registry (not the Registerer
// interface) to avoid the typed-nil trap a nil registry would otherwise
// hide behind a non-nil interface value.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		PublishedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "live",
			Name:      "events_published_total",
			Help:      "Live execution events published, by event kind.",
		}, []string{"kind"}),
		DroppedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "live",
			Name:      "events_dropped_total",
			Help:      "Live execution events dropped to a slow subscriber during non-blocking fan-out, by reason.",
		}, []string{"reason"}),
		DetachedTimeoutTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "live",
			Name:      "event_detached_timeout_total",
			Help:      "DB-backed live-event publish phases that ran out their own bound, by phase (append = lost to other replicas; notify = delayed until catch-up).",
		}, []string{"phase"}),
	}
	reg.MustRegister(m.PublishedTotal, m.DroppedTotal, m.DetachedTimeoutTotal)
	return m
}

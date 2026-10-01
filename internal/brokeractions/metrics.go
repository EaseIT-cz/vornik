package brokeractions

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Metrics holds the broker-action series (design §5.4). Created before the
// observability registry exists and attached to it later, like the
// tool-audit census: every method is a no-op until Attach, and nil-safe.
type Metrics struct {
	mu           sync.Mutex
	stuck        *prometheus.GaugeVec
	finished     *prometheus.CounterVec
	finishFailed prometheus.Counter
	unscanned    *prometheus.CounterVec
}

// NewMetrics returns an unattached holder.
func NewMetrics() *Metrics { return &Metrics{} }

// Attach registers the series once; later calls are no-ops.
func (m *Metrics) Attach(registerer prometheus.Registerer, logger *zerolog.Logger) {
	if m == nil || registerer == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stuck != nil {
		return
	}
	f := promauto.With(registerer)
	m.stuck = f.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vornik",
		Subsystem: "broker",
		Name:      "actions_stuck",
		Help: "Broker write actions no one is finishing, by project and status: executing or unknown " +
			"for over 15 minutes (an operator must resolve them with vornikctl broker-action resolve) " +
			"and approved for over 5 minutes (the action worker is not running). Set on each worker " +
			"sweep; absent series mean the worker has not swept, not that nothing is stuck.",
	}, []string{"project", "status"})
	m.finished = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "broker",
		Name:      "actions_finished_total",
		Help: "Broker write actions the worker finished, by outcome_class: ok, tool_error, " +
			"pre_send_error (refused or not sent), timeout and transport_error (both left unknown " +
			"for an operator). Operator resolutions are not counted here.",
	}, []string{"outcome_class"})
	m.finishFailed = f.NewCounter(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "broker",
		Name:      "actions_finish_failed_total",
		Help: "Broker write actions whose outcome the worker could not record (the store refused " +
			"or failed the terminal write). Each row stays executing and shows in " +
			"vornik_broker_actions_stuck after 15 minutes; these are not in actions_finished_total.",
	})
	m.unscanned = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "broker",
		Name:      "actions_outcomes_unscanned_total",
		Help: "Tool responses stored as broker-action outcomes without a secret scan, by reason: " +
			"secrets_disabled (secret scanning is off) or detector_unavailable (it is on but its " +
			"detector failed to build). Zero with actions finishing means every outcome was scanned.",
	}, []string{"reason"})
	if logger != nil {
		logger.Debug().Msg("broker actions: metrics attached")
	}
}

// SetStuck replaces the stuck gauge with one sweep's counts, so a project
// whose rows cleared drops to no series rather than keeping its last value.
func (m *Metrics) SetStuck(rows []persistence.BrokerActionStuck) {
	if m == nil {
		return
	}
	m.mu.Lock()
	g := m.stuck
	m.mu.Unlock()
	if g == nil {
		return
	}
	g.Reset()
	for _, r := range rows {
		g.WithLabelValues(r.ProjectID, r.Status).Set(float64(r.Count))
	}
}

// RecordFinished counts one worker-finished action.
func (m *Metrics) RecordFinished(outcomeClass string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.finished
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.WithLabelValues(outcomeClass).Inc()
}

// RecordFinishFailed counts one outcome the worker could not record.
func (m *Metrics) RecordFinishFailed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.finishFailed
	m.mu.Unlock()
	if c != nil {
		c.Inc()
	}
}

// RecordUnscanned counts one outcome stored without a secret scan.
func (m *Metrics) RecordUnscanned(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.unscanned
	m.mu.Unlock()
	if c != nil {
		c.WithLabelValues(reason).Inc()
	}
}

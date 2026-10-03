package brokergrants

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
)

// Metrics holds the standing-grant series (tier 2 revised item 10, round 3,
// round 4 F5). Usable before Attach: the series are built at NewMetrics and
// registered when the registry exists. Nil-safe.
type Metrics struct {
	mu        sync.Mutex
	attached  bool
	live      *prometheus.GaugeVec
	paused    *prometheus.GaugeVec
	covered   *prometheus.CounterVec
	revoked   prometheus.Counter
	suspended prometheus.Counter
	miss      *prometheus.CounterVec
}

// Miss reasons (round 4 F5): an action matched a grant but fell back to a
// per-write approval.
const (
	MissRevoked   = "revoked"
	MissPaused    = "paused"
	MissSuspended = "suspended"
	MissExpired   = "expired"
	MissUsed      = "used"
	MissKey       = "key"
	// MissUnkeyed: the action's key could not be computed (a destination
	// that is not address-shaped), so no grant was tried (review f819).
	MissUnkeyed = "unkeyed"
	// MissSuspendFailed: the grant had to be suspended (its workflow's
	// reach changed) but the store refused the suspension; it covered
	// nothing (review f819).
	MissSuspendFailed = "suspend_failed"
)

// NewMetrics builds the series, unregistered.
func NewMetrics() *Metrics {
	f := promauto.With(nil)
	return &Metrics{
		live: f.NewGaugeVec(prometheus.GaugeOpts{Namespace: "vornik", Subsystem: "broker", Name: "standing_grants_live",
			Help: "Live standing grants by project: not revoked, not expired, uses left (paused and suspended ones included). " +
				"Replaced on each housekeeping pass; absent series mean no pass has run, not that none exist."}, []string{"project"}),
		paused: f.NewGaugeVec(prometheus.GaugeOpts{Namespace: "vornik", Subsystem: "broker", Name: "standing_grants_paused",
			Help: "Live standing grants a person paused, by project. Replaced on each housekeeping pass."}, []string{"project"}),
		covered: f.NewCounterVec(prometheus.CounterOpts{Namespace: "vornik", Subsystem: "broker", Name: "actions_covered_total",
			Help: "Broker write actions approved under a standing grant (no per-write approval; their text was not shown to a person), by project."}, []string{"project"}),
		revoked: f.NewCounter(prometheus.CounterOpts{Namespace: "vornik", Subsystem: "broker", Name: "standing_grants_revoked_total",
			Help: "Standing grants a person revoked."}),
		suspended: f.NewCounter(prometheus.CounterOpts{Namespace: "vornik", Subsystem: "broker", Name: "standing_grants_suspended_total",
			Help: "Standing grants suspended because their workflow's approved reach changed after they were created."}),
		miss: f.NewCounterVec(prometheus.CounterOpts{Namespace: "vornik", Subsystem: "broker", Name: "standing_grants_match_miss_total",
			Help: "Broker write actions whose key matched a standing grant that could not cover them, so they fell back to a per-write " +
				"approval, by reason: revoked, paused, suspended, expired, used, key (the guarded decrement refused the key or class), " +
				"unkeyed (the write's key could not be computed) or suspend_failed (a reach change could not be recorded). " +
				"An action matching no grant is not counted."}, []string{"reason"}),
	}
}

// Attach registers the series once.
func (m *Metrics) Attach(reg prometheus.Registerer) {
	if m == nil || reg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attached {
		return
	}
	m.attached = true
	for _, c := range []prometheus.Collector{m.live, m.paused, m.covered, m.revoked, m.suspended, m.miss} {
		_ = reg.Register(c)
	}
}

// SetLive replaces both gauges with one pass's counts.
func (m *Metrics) SetLive(rows []persistence.BrokerGrantCount) {
	if m == nil {
		return
	}
	m.live.Reset()
	m.paused.Reset()
	for _, r := range rows {
		m.live.WithLabelValues(r.ProjectID).Set(float64(r.Live))
		m.paused.WithLabelValues(r.ProjectID).Set(float64(r.Paused))
	}
}

func (m *Metrics) recordCovered(project string) {
	if m != nil {
		m.covered.WithLabelValues(project).Inc()
	}
}

func (m *Metrics) recordRevoked() {
	if m != nil {
		m.revoked.Inc()
	}
}

func (m *Metrics) recordSuspended() {
	if m != nil {
		m.suspended.Inc()
	}
}

func (m *Metrics) recordMiss(reason string) {
	if m != nil && reason != "" {
		m.miss.WithLabelValues(reason).Inc()
	}
}

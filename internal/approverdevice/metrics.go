package approverdevice

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HostActionMetrics holds vornik_host_approvals_total (Hermes approval
// transport design §8). Created with the service, before the observability
// registry exists, and attached later; nil-safe and a no-op until attached.
type HostActionMetrics struct {
	mu    sync.Mutex
	total *prometheus.CounterVec
}

// NewHostActionMetrics returns an unattached holder.
func NewHostActionMetrics() *HostActionMetrics { return &HostActionMetrics{} }

// Attach registers the counter once; later calls are no-ops.
func (m *HostActionMetrics) Attach(registerer prometheus.Registerer) {
	if m == nil || registerer == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total != nil {
		return
	}
	m.total = promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "host",
		Name:      "approvals_total",
		Help: "Host approval requests (an agent host's own safety prompt answered on the approver " +
			"device) by harness and outcome: filed, once, session, deny (the phone's answers), " +
			"expired (no answer by Vornik's deadline), refused_cap (over 3 pending or 30 an hour) and " +
			"conflict (a refile with another digest). A request the host denied because Vornik was " +
			"unreachable is not counted here; the host plugin's log records it.",
	}, []string{"harness", "outcome"})
}

// Record counts one outcome.
func (m *HostActionMetrics) Record(harness, outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.total
	m.mu.Unlock()
	if c != nil {
		c.WithLabelValues(harness, outcome).Inc()
	}
}

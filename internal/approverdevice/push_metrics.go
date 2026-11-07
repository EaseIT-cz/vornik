package approverdevice

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PushMetrics holds vornik_approver_push_total (GitHub #163, design part 09
// §9.3a B.5). Like HostActionMetrics it is created with the service and
// attached once the registry exists; nil-safe and a no-op until attached.
type PushMetrics struct {
	mu    sync.Mutex
	total *prometheus.CounterVec
}

// NewPushMetrics returns an unattached holder.
func NewPushMetrics() *PushMetrics { return &PushMetrics{} }

// Attach registers the counter once; later calls are no-ops.
func (m *PushMetrics) Attach(registerer prometheus.Registerer) {
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
		Subsystem: "approver",
		Name:      "push_total",
		Help: "Web Push notifications to approver devices by outcome: sent (the push service " +
			"accepted it), gone (the browser dropped the subscription, which is deleted) and failed " +
			"(any other answer, a redirect, no answer or a sender error; not retried).",
	}, []string{"outcome"})
}

// Record counts one outcome.
func (m *PushMetrics) Record(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.total
	m.mu.Unlock()
	if c != nil {
		c.WithLabelValues(outcome).Inc()
	}
}

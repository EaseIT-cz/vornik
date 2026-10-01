package companionpush

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds vornik_companion_push_total (design §7a). Created before the
// observability registry exists and attached later; nil-safe and a no-op
// until attached.
type Metrics struct {
	mu    sync.Mutex
	total *prometheus.CounterVec
}

// NewMetrics returns an unattached holder.
func NewMetrics() *Metrics { return &Metrics{} }

// Attach registers the counter once; later calls are no-ops.
func (m *Metrics) Attach(registerer prometheus.Registerer) {
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
		Subsystem: "companion",
		Name:      "push_total",
		Help: "Companion push outcomes by kind (task|action) and result: delivered, failed (the " +
			"receiver answered non-2xx or the connection failed; retried next pass), refused (the " +
			"reach guard: not public and not in the project's companion_push.allowed_cidrs), and " +
			"abandoned (10 failed passes; dropped on this node until it restarts).",
	}, []string{"kind", "result"})
}

// Record counts one outcome.
func (m *Metrics) Record(kind, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.total
	m.mu.Unlock()
	if c != nil {
		c.WithLabelValues(kind, result).Inc()
	}
}

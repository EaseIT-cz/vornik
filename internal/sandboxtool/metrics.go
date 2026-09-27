package sandboxtool

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics are the §7.4 signals. A rising oom or timeout rate for one tool is
// how an operator sees a crafted-file DoS in progress.
type Metrics struct {
	runs           *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	wait           *prometheus.HistogramVec
	removeFailures prometheus.Counter
}

// NewMetrics registers vornik_sandbox_tool_runs_total,
// vornik_sandbox_tool_duration_seconds (the run alone),
// vornik_sandbox_tool_wait_seconds (the pool queue, S5a review F7) and
// vornik_sandbox_tool_remove_failures_total (S5a review F2). A nil registerer
// uses the default.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		runs: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Name: "vornik_sandbox_tool_runs_total",
			Help: "Sandbox one-shot tool runs by feature and outcome (ok, failed, timeout, oom, not_available).",
		}, []string{"tool", "outcome"}),
		duration: promauto.With(registerer).NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vornik_sandbox_tool_duration_seconds",
			Help:    "Wall time of a sandbox one-shot tool run, including container start, excluding the pool queue.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"tool"}),
		wait: promauto.With(registerer).NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vornik_sandbox_tool_wait_seconds",
			Help:    "Time a sandbox one-shot waited for a pool slot before starting.",
			Buckets: []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"tool"}),
		removeFailures: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name: "vornik_sandbox_tool_remove_failures_total",
			Help: "Sandbox run containers podman failed to remove; the next daemon start sweeps them.",
		}),
	}
}

func (m *Metrics) observe(f Feature, o Outcome, d time.Duration) {
	m.runs.WithLabelValues(string(f), string(o)).Inc()
	m.duration.WithLabelValues(string(f)).Observe(d.Seconds())
}

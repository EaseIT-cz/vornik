package artifacts

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// promFindingsCounter is the Prometheus FindingsCounter the daemon wires
// (GitHub #85, artifact heuristic-redaction design §7).
type promFindingsCounter struct{ c *prometheus.CounterVec }

// NewPromFindingsCounter registers vornik_artifact_secret_findings_total on
// reg. Its labels are the rule type (built-in or operator patterns, fixed at
// config load) and the outcome, so its cardinality is bounded.
func NewPromFindingsCounter(reg prometheus.Registerer) FindingsCounter {
	return promFindingsCounter{c: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Name:      "artifact_secret_findings_total",
		Help: "Secret findings in stored artifacts, by rule type and outcome: rewritten (redacted), " +
			"stored (detect mode, unchanged) or dropped (a heuristic rule, never rewritten on this surface).",
	}, []string{"type", "outcome"})}
}

func (p promFindingsCounter) Inc(ruleType, outcome string) {
	p.c.WithLabelValues(ruleType, outcome).Inc()
}

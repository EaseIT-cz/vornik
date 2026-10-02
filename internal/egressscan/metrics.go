package egressscan

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Surfaces a scan runs on (plan P5).
const (
	SurfaceToolArgs   = "tool_args"
	SurfaceAPIArgs    = "api_args"
	SurfaceActionArgs = "action_args"
	SurfaceEgressDoc  = "egress_doc"
)

// Metrics counts what each surface examined as well as what it found, so a
// quiet surface reads as "examined N, found 0", not as silence. Nil-safe.
type Metrics struct {
	mu       sync.Mutex
	examined *prometheus.CounterVec
	findings *prometheus.CounterVec
}

// NewMetrics returns unattached metrics; Observe is a no-op until Attach.
func NewMetrics() *Metrics { return &Metrics{} }

// Attach registers the series once; later calls are no-ops.
func (m *Metrics) Attach(registerer prometheus.Registerer) {
	if m == nil || registerer == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.examined != nil {
		return
	}
	f := promauto.With(registerer)
	m.examined = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Subsystem: "egress", Name: "secret_examined_total",
		Help: "Outbound documents the egress secret scan examined, by surface (tool_args, api_args, action_args, egress_doc). The denominator for vornik_egress_secret_findings_total.",
	}, []string{"surface"})
	m.findings = f.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Subsystem: "egress", Name: "secret_findings_total",
		Help: "Credential-shaped values the egress secret scan found in outbound data, by surface, project, detector type and the action taken (block, redact, detect). The project label is what a per-project detect-to-redact decision reads (Part A section 3.2). The value is never a label.",
	}, []string{"surface", "project", "type", "action"})
}

// Observe records one examined document and its findings.
func (m *Metrics) Observe(surface, projectID string, fs []Finding, action string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	examined, findings := m.examined, m.findings
	m.mu.Unlock()
	if examined == nil {
		return
	}
	examined.WithLabelValues(surface).Inc()
	for _, f := range fs {
		findings.WithLabelValues(surface, projectID, f.Type, action).Inc()
	}
}

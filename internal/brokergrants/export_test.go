package brokergrants

import "github.com/prometheus/client_golang/prometheus/testutil"

// Covered reads the covered counter (tests and the doctor).
func (m *Metrics) Covered(project string) float64 {
	return testutil.ToFloat64(m.covered.WithLabelValues(project))
}

// Miss reads one miss reason's counter.
func (m *Metrics) Miss(reason string) float64 {
	return testutil.ToFloat64(m.miss.WithLabelValues(reason))
}

// Revoked reads the revoke counter.
func (m *Metrics) Revoked() float64 { return testutil.ToFloat64(m.revoked) }

// Suspended reads the suspension counter.
func (m *Metrics) Suspended() float64 { return testutil.ToFloat64(m.suspended) }

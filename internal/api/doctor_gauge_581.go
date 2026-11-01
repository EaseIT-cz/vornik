package api

// The doctor gauge (GitHub #581 D6, enterprise-packaging-design.md amendment
// 2026-10-29). A managed instance runs the read-only doctor report every five
// minutes and once at start; the control plane alerts on vornik_doctor_errors
// with the scrape token and never calls doctor. Triage reads `vornikctl
// doctor` on the instance.

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DoctorGaugeInterval is how often a managed instance refreshes the gauges.
const DoctorGaugeInterval = 5 * time.Minute

// doctorSeverities are the statuses a DoctorCheck carries, as gauge labels.
var doctorSeverities = []string{"ok", "warning", "error", "skipped"}

var (
	doctorErrorsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "vornik", Name: "doctor_errors",
		Help: "Error-severity checks in the last read-only doctor report (managed hosting; refreshed every 5 minutes).",
	})
	doctorChecksGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vornik", Name: "doctor_checks",
		Help: "Checks in the last read-only doctor report, by severity (ok, warning, error, skipped).",
	}, []string{"severity"})
)

// recordDoctorGauges sets the gauges to report's counts. Every severity is
// set, so one that has gone to zero reads zero, not its last value.
func recordDoctorGauges(report DoctorReport) {
	counts := make(map[string]int, len(doctorSeverities))
	for _, c := range report.Checks {
		counts[strings.ToLower(c.Status)]++
	}
	for _, sev := range doctorSeverities {
		doctorChecksGauge.WithLabelValues(sev).Set(float64(counts[sev]))
	}
	doctorErrorsGauge.Set(float64(counts["error"]))
}

// RunDoctorGaugeLoop refreshes the gauges from h's read-only report, at start
// and every DoctorGaugeInterval, until ctx ends.
func (h *DoctorHandlers) RunDoctorGaugeLoop(ctx context.Context) {
	runDoctorGaugeLoop(ctx, DoctorGaugeInterval, h.RunReportReadOnly)
}

func runDoctorGaugeLoop(ctx context.Context, every time.Duration, report func(context.Context) DoctorReport) {
	recordDoctorGauges(safeDoctorReport(ctx, report))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			recordDoctorGauges(safeDoctorReport(ctx, report))
		}
	}
}

// safeDoctorReport runs report, turning a panic into a report of one error
// check. The HTTP doctor's panics are recovered by the router; this loop has
// no such guard, and a check that panics must raise the alert, not stop the
// daemon.
func safeDoctorReport(ctx context.Context, report func(context.Context) DoctorReport) (r DoctorReport) {
	defer func() {
		if p := recover(); p != nil {
			r = DoctorReport{Checks: []DoctorCheck{{Name: "doctor_gauge", Status: "ERROR", Message: "a doctor check panicked; run vornikctl doctor"}}}
		}
	}()
	return report(ctx)
}

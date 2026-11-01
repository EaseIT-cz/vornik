package api

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// GitHub #581 D6 (enterprise-packaging-design.md, amendment 2026-10-29): the
// control plane alerts on vornik_doctor_errors and never calls doctor, so the
// gauge must count the error-severity checks of the last report.
func TestDoctorGauge581_CountsTheErrorChecks(t *testing.T) {
	recordDoctorGauges(DoctorReport{Checks: []DoctorCheck{
		{Name: "a", Status: "OK"},
		{Name: "b", Status: "ERROR"},
		{Name: "c", Status: "WARNING"},
		{Name: "d", Status: "WARNING"},
		{Name: "e", Status: "SKIPPED"},
	}})
	if got := testutil.ToFloat64(doctorErrorsGauge); got != 1 {
		t.Errorf("vornik_doctor_errors = %v; want 1", got)
	}
	for sev, want := range map[string]float64{"ok": 1, "error": 1, "warning": 2, "skipped": 1} {
		if got := testutil.ToFloat64(doctorChecksGauge.WithLabelValues(sev)); got != want {
			t.Errorf("vornik_doctor_checks{severity=%q} = %v; want %v", sev, got, want)
		}
	}

	// A later clean report brings both down: the gauges are the last
	// report's, not a running total.
	recordDoctorGauges(DoctorReport{Checks: []DoctorCheck{{Name: "a", Status: "OK"}}})
	if got := testutil.ToFloat64(doctorErrorsGauge); got != 0 {
		t.Errorf("vornik_doctor_errors after a clean report = %v; want 0", got)
	}
	if got := testutil.ToFloat64(doctorChecksGauge.WithLabelValues("warning")); got != 0 {
		t.Errorf("vornik_doctor_checks{severity=\"warning\"} after a clean report = %v; want 0", got)
	}
}

// The loop reports once at start, before the first tick, and stops with its
// context.
func TestDoctorGauge581_TheLoopReportsAtStartAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runs := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		runDoctorGaugeLoop(ctx, time.Hour, func(context.Context) DoctorReport {
			runs <- struct{}{}
			return DoctorReport{Checks: []DoctorCheck{{Name: "x", Status: "ERROR"}}}
		})
		close(done)
	}()
	select {
	case <-runs:
	case <-time.After(5 * time.Second):
		t.Fatal("no report at start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
}

// A check that panics raises the alert and leaves the loop running: the loop
// has no router to recover it, and a panic there would stop the daemon.
func TestDoctorGauge581_APanickingReportCountsAsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 64)
	done := make(chan struct{})
	go func() {
		runDoctorGaugeLoop(ctx, 10*time.Millisecond, func(context.Context) DoctorReport {
			select {
			case calls <- struct{}{}:
			default:
			}
			panic("a check failed hard")
		})
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d did not run; the panic stopped the loop", i+1)
		}
	}
	cancel()
	// Wait for the loop to exit, so no write lands after the test (0f9d #8).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
	if got := testutil.ToFloat64(doctorErrorsGauge); got != 1 {
		t.Errorf("vornik_doctor_errors after a panic = %v; want 1", got)
	}
}

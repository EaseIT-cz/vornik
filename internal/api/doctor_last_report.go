package api

import (
	"sync"
	"time"
)

// doctorLastReport is what the overview shows of doctor (web UI design,
// "The overview answers...", 2026-11-07, GitHub #603): the names of the
// error checks in the last report any caller ran, and when. Doctor takes 8 to
// 17 s on the reference host, so the overview never runs it; it shows the
// last report's errors with their age, or nothing before the first report.
type doctorLastReport struct {
	mu     sync.Mutex
	at     time.Time
	errors []string
}

// remember keeps report's error-check names and the time it finished.
func (l *doctorLastReport) remember(report DoctorReport, at time.Time) {
	var names []string
	for _, c := range report.Checks {
		if c.Status == "ERROR" {
			names = append(names, c.Name)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.errors = at, names
}

// LastReportErrors returns the error-check names of the last doctor report and
// when it finished; ok is false before the first report since start.
func (h *DoctorHandlers) LastReportErrors() (names []string, at time.Time, ok bool) {
	h.lastReport.mu.Lock()
	defer h.lastReport.mu.Unlock()
	if h.lastReport.at.IsZero() {
		return nil, time.Time{}, false
	}
	return append([]string(nil), h.lastReport.errors...), h.lastReport.at, true
}

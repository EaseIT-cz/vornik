package api

import (
	"sync"
	"testing"
	"time"
)

// GitHub #603: the overview shows the error checks of the last doctor report
// any caller ran, never running doctor itself. Before the first report there
// is nothing to show; a later report replaces the earlier one.
func TestDoctorLastReport_603(t *testing.T) {
	h := &DoctorHandlers{}
	if _, _, ok := h.LastReportErrors(); ok {
		t.Fatal("no report has run, so there is nothing to show")
	}
	at := time.Now().Add(-time.Hour)
	h.lastReport.remember(DoctorReport{Checks: []DoctorCheck{
		{Name: "model_health", Status: "ERROR"}, {Name: "image_freshness", Status: "WARNING"},
		{Name: "fallback_rungs", Status: "ERROR"}, {Name: "database", Status: "OK"},
	}}, at)
	names, got, ok := h.LastReportErrors()
	if !ok || !got.Equal(at) || len(names) != 2 || names[0] != "model_health" || names[1] != "fallback_rungs" {
		t.Fatalf("got %v %v %v, want the two ERROR checks at %v", names, got, ok, at)
	}
	names[0] = "mutated"
	if again, _, _ := h.LastReportErrors(); again[0] != "model_health" {
		t.Fatal("the caller's slice must not alias the stored report")
	}
	h.lastReport.remember(DoctorReport{Checks: []DoctorCheck{{Name: "database", Status: "OK"}}}, time.Now())
	if names, _, ok := h.LastReportErrors(); !ok || len(names) != 0 {
		t.Fatalf("a clean report replaces the earlier one: %v %v", names, ok)
	}
}

// Doctor runs from the CLI, the API and the managed gauge loop, so reports can
// finish at once while the overview reads; run under -race.
func TestDoctorLastReport_603_Concurrent(t *testing.T) {
	h := &DoctorHandlers{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.lastReport.remember(DoctorReport{Checks: []DoctorCheck{{Name: "a", Status: "ERROR"}, {Name: "b", Status: "ERROR"}}}, time.Now())
		}()
		go func() {
			defer wg.Done()
			if names, _, ok := h.LastReportErrors(); ok && len(names) != 2 {
				t.Errorf("a torn read: %v", names)
			}
		}()
	}
	wg.Wait()
}

package api

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BreachLedger is the one read the Art 33 deadline check needs from the breach
// ledger. subjectstore.IncidentRepository satisfies it on both drivers
// (GitHub #475; GDPR design §4.21).
type BreachLedger interface {
	CountLiveByDeadline(ctx context.Context, now time.Time) (overdue, soon int, err error)
}

// SetBreachLedger wires the ledger behind checkBreachDeadlines. The container
// passes the driver-resolved repository it gives the regulatory page, so the
// doctor and the page read one ledger. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetBreachLedger(l BreachLedger) {
	if h == nil {
		return
	}
	h.breachLedger = l
}

// checkBreachDeadlines surfaces undischarged Art 33 breach notifications.
//
// The whole failure mode this guards is a clock nobody is watching: GDPR
// Art 33(1) gives 72 hours from BECOMING AWARE of a personal-data breach to
// notify the supervisory authority, and a missed deadline is not recoverable —
// you cannot un-miss it, you can only explain it. So the check warns while there
// is still a day left, and escalates to ERROR once the window has closed.
//
// "Undischarged" rather than "un-notified": recording that a breach is not
// notifiable, with its ground, discharges Art 33 as fully as notifying does.
// An incident sitting in 'detected' is the dangerous state, because nobody has
// decided anything about it yet.
//
// see LLD § https://docs.vornik.io §4.10
func (h *DoctorHandlers) checkBreachDeadlines(ctx context.Context) DoctorCheck {
	name := "breach_deadlines"

	if h == nil || h.breachLedger == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "breach ledger not wired into the doctor (no database, or an unsupported driver)"}
	}
	overdue, soon, err := h.breachLedger.CountLiveByDeadline(ctx, time.Now())
	if err != nil {
		// A database predating the ledger's schema (Postgres migration 143, or
		// a SQLite file from before GitHub #475 that has not migrated) has no
		// table; that is not a finding, and reporting it as one would train
		// operators to ignore this check. Nor may the driver's error be the
		// verdict (doctor design, Extension 2026-09-04 E2): "no such column:
		// state" tells an operator nothing. SKIPPED, with the reason in
		// operator language; the raw error stays in Items for diagnosis.
		msg := "breach ledger not present on this database (it predates the ledger's " +
			"schema), so no Art 33 deadline was examined"
		if !isMissingRelation(err) {
			msg = "breach ledger could not be read, so no Art 33 deadline was examined"
		}
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: msg,
			Items: []string{fmt.Sprintf("underlying error: %v", err)}}
	}

	switch {
	case overdue > 0:
		return DoctorCheck{Name: name, Status: "ERROR",
			Message: fmt.Sprintf("%d personal-data breach(es) past the Art 33(1) 72-hour notification "+
				"deadline with no recorded decision. This cannot be un-missed — notify the supervisory "+
				"authority now, or record why notification is not owed, and document the delay: Art 33(1) "+
				"requires reasons for a late notification. See: vornikctl incident list", overdue)}
	case soon > 0:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: fmt.Sprintf("%d personal-data breach(es) approaching the Art 33(1) 72-hour deadline. "+
				"Assess and decide now — a notification needs facts gathered and a risk judgement made, "+
				"neither of which compresses well. See: vornikctl incident list", soon)}
	default:
		return DoctorCheck{Name: name, Status: "OK",
			Message: "no personal-data breaches within the Art 33 notification window"}
	}
}

// isMissingRelation reports whether err is a database saying the table or
// column simply is not there — the expected shape on a pre-migration
// deployment and on a driver this store was never built for. Distinguished
// from a real failure so the latter keeps its detail in the verdict.
func isMissingRelation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"no such table",    // sqlite
		"no such column",   // sqlite
		"does not exist",   // postgres: relation/column does not exist
		"undefined_table",  // postgres sqlstate name
		"undefined_column", //
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// SubjectRequestLedger is the one read the Art 12(3) deadline check needs from
// the data-subject request ledger. subjectstore.Repository satisfies it on both
// drivers.
type SubjectRequestLedger interface {
	ListLiveRequests(ctx context.Context) ([]datasubject.Request, error)
}

// SetSubjectRequestLedger wires the ledger behind checkSubjectRequestDeadlines.
// The container passes the same driver-resolved repository it gives the
// regulatory page, so the doctor and the page read one ledger. Nil leaves the
// check SKIPPED.
func (h *DoctorHandlers) SetSubjectRequestLedger(l SubjectRequestLedger) {
	if h == nil {
		return
	}
	h.subjectRequests = l
}

// checkSubjectRequestDeadlines surfaces data-subject rights requests at or near
// their GDPR Art 12(3) response deadline.
//
// Until this check the clock was visible only to an operator who went looking:
// `vornikctl subject requests` and the regulatory record page. A deadline that
// nobody opens a page for is missed silently. The design promised a doctor
// warning (§4.8) and recorded on 2026-09-25 that none had been built.
//
// It classifies with the domain's own Overdue and NeedsAttention, so the check
// cannot disagree with the CLI or the page. It never names the subject: the
// doctor report is embedded in support bundles, and a request id with its kind
// is enough for the operator to find the row.
//
// see LLD § https://docs.vornik.io §4.20
func (h *DoctorHandlers) checkSubjectRequestDeadlines(ctx context.Context) DoctorCheck {
	name := "subject_request_deadlines"
	if h == nil || h.subjectRequests == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "subject-request ledger not wired into the doctor (no database, or an unsupported driver)"}
	}
	reqs, err := h.subjectRequests.ListLiveRequests(ctx)
	if err != nil {
		// A driver error is never a verdict (doctor design, Extension
		// 2026-09-04 E2): the reason in operator language, the raw error
		// kept in Items for diagnosis.
		msg := "subject-request ledger could not be read, so no Art 12(3) deadline was examined"
		if isMissingRelation(err) {
			msg = "subject-request ledger not present on this database (it predates the ledger's " +
				"schema), so no Art 12(3) deadline was examined"
		}
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: msg,
			Items: []string{fmt.Sprintf("underlying error: %v", err)}}
	}
	if len(reqs) == 0 {
		return DoctorCheck{Name: name, Status: "OK",
			Message: "no live rights requests (none open or verified awaiting a response)"}
	}

	now := time.Now().UTC()
	var overdue, soon []string
	for _, r := range reqs {
		line := fmt.Sprintf("%s %s %s due %s", r.ID, r.Kind, r.State, r.Deadline().UTC().Format(time.RFC3339))
		switch {
		case r.Overdue(now):
			overdue = append(overdue, line+" (OVERDUE)")
		case r.NeedsAttention(now):
			soon = append(soon, line)
		}
	}
	items := append(append([]string{}, overdue...), soon...)
	warnDays := int(datasubject.WarnBefore / (24 * time.Hour))

	switch {
	case len(overdue) > 0:
		return DoctorCheck{Name: name, Status: "ERROR", Items: items,
			Message: fmt.Sprintf("%d rights request(s) past the GDPR Art 12(3) response deadline, of %d live. "+
				"The response is now late: answer the subject, or record the refusal and its ground, and "+
				"document why it was late. See: vornikctl subject requests", len(overdue), len(reqs))}
	case len(soon) > 0:
		return DoctorCheck{Name: name, Status: "WARNING", Items: items,
			Message: fmt.Sprintf("%d rights request(s) within %d days of the GDPR Art 12(3) response deadline, "+
				"of %d live. Act now, or extend under Art 12(3) and tell the subject within the first month. "+
				"See: vornikctl subject requests", len(soon), warnDays, len(reqs))}
	default:
		return DoctorCheck{Name: name, Status: "OK",
			Message: fmt.Sprintf("%d live rights request(s) examined; none within %d days of its Art 12(3) deadline",
				len(reqs), warnDays)}
	}
}

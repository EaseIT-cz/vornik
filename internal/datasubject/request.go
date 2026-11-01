package datasubject

import (
	"fmt"
	"strings"
	"time"
)

// RequestKind is the right being exercised.
type RequestKind string

// The rights a subject may exercise. Portability and access differ in scope
// (Art 20 covers only data the subject provided under consent or contract),
// which is why they are separate kinds rather than one "export".
const (
	RequestAccess        RequestKind = "access"        // Art 15
	RequestRectification RequestKind = "rectification" // Art 16
	RequestErasure       RequestKind = "erasure"       // Art 17
	RequestRestriction   RequestKind = "restriction"   // Art 18
	RequestPortability   RequestKind = "portability"   // Art 20
	RequestObjection     RequestKind = "objection"     // Art 21
)

// RequestState is where a request has got to.
type RequestState string

const (
	// StateOpen — recorded, clock running, identity NOT yet established.
	StateOpen RequestState = "open"
	// StateVerified — the requester's identity is established (Art 12(6)).
	// This is the only state from which data may be produced or erased.
	StateVerified RequestState = "verified"
	// StateActioned — the right has been exercised; the report exists.
	StateActioned RequestState = "actioned"
	// StateRefused — refused with a reason, which is itself a response the
	// subject may challenge. A refusal is an outcome, not an absence of one.
	StateRefused RequestState = "refused"
	// StateClosed — terminal.
	StateClosed RequestState = "closed"
)

// Art 12(3) warning margins. The deadline itself is counted in calendar
// months (Deadline, monthsAfter), never as a fixed duration (#590).
const (
	// WarnBefore is how long before the deadline an operator should be warned.
	// Nine days: the warning has to leave time to actually DO the work, not
	// merely to notice the miss.
	WarnBefore = 9 * 24 * time.Hour
	// EscalationMargin is how long before the deadline a transcript row that a
	// running task still holds is pushed to the operator (O-5d-3, GitHub #344
	// 5d-1c). Not WarnBefore: that is the general 9-day warning doctor shows
	// for every open request; this is a push alert for one deferred row.
	EscalationMargin = 7 * 24 * time.Hour
)

// Request is one rights request and its audit trail.
type Request struct {
	ID        string
	SubjectID string
	Kind      RequestKind
	State     RequestState
	// OpenedAt is when the request was RECEIVED, in UTC: the Art 12(3) clock
	// runs from receipt, not from when it was recorded (#590).
	OpenedAt time.Time

	// VerifiedBy / VerifiedHow record WHO established identity and HOW.
	// Both are required to leave StateOpen: an unattributable verification is
	// indistinguishable from none, and handing an export to an unverified
	// requester is itself a personal-data breach.
	VerifiedBy  string
	VerifiedHow string
	VerifiedAt  time.Time

	// Extended records an Art 12(3) extension. The extension is only lawful if
	// the subject was told about it within the first month, so the reason is
	// recorded for the accountability trail.
	Extended       bool
	ExtendedReason string
	// ExtensionNotifiedAt is when the subject was told of the extension, with
	// its reasons (GitHub #344, 5d-1c). A separate fact from Extended: the
	// extension is lawful only if this is within the first month.
	ExtensionNotifiedAt time.Time

	// ReportHash pins the artefact that was produced, so "what did we actually
	// send them" is answerable later (Art 5(2)).
	ReportHash string
	// ReportRetained reports whether the report itself (not only its hash) was
	// kept — set by the regulatory record's listing so it links a download only
	// where one exists (regulatory record design §4). Erasure retains its
	// report; an Art 15/20 export today keeps only the hash.
	ReportRetained bool

	// RefusedReason is required in StateRefused. A refusal with no stated
	// ground is the shape of obstruction, and Art 12(4) requires the subject be
	// told why.
	RefusedReason string

	// ErasureGround is the Art 17(1) limb an erasure request is made under.
	// Captured at INTAKE, because it decides whether a row that also concerns
	// another person may survive redacted or must be deleted outright
	// (design §5.3). Required for RequestErasure and meaningless otherwise —
	// PlanErasure refuses rather than inferring it, since inferring the ground
	// would mean inferring whether a third party's data is destroyed.
	ErasureGround ErasureGround
}

// Deadline is when the Art 12(3) response is due: one month from receipt,
// or, extended, two further months from the end of the first (GDPR rights
// design §4.20.6).
func (r Request) Deadline() time.Time {
	first := FirstMonthEnd(r.OpenedAt)
	if r.Extended {
		return monthsAfter(first, 2)
	}
	return first
}

// monthsAfter is the end of a period of n months from t (Regulation 1182/71
// Art 3(2)(c)): the last instant of the UTC day in the n-th month after t's
// month with t's UTC date, or of that month's last day when it has none.
// Art 3(4) (a last day on a weekend or holiday moves to the next working
// day) is not applied; it only ever makes the period longer.
func monthsAfter(t time.Time, n int) time.Time {
	t = t.UTC()
	y, m, d := t.Date()
	firstOfTarget := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	if last := firstOfTarget.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return time.Date(firstOfTarget.Year(), firstOfTarget.Month(), d, 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, 1).Add(-time.Nanosecond)
}

// Overdue reports whether the response deadline has passed unmet.
func (r Request) Overdue(now time.Time) bool {
	if r.State == StateActioned || r.State == StateRefused || r.State == StateClosed {
		return false
	}
	return now.After(r.Deadline())
}

// NeedsAttention reports whether the operator should be warned. Deliberately
// fires BEFORE the deadline rather than on it: a warning that arrives the day a
// legal deadline expires is a notification of failure, not a prompt to act.
func (r Request) NeedsAttention(now time.Time) bool {
	if r.State == StateActioned || r.State == StateRefused || r.State == StateClosed {
		return false
	}
	return now.After(r.Deadline().Add(-WarnBefore))
}

// MayProduceData reports whether this request has cleared the identity gate.
//
// The gate is the whole point: producing an Art 15 export for an unverified
// requester discloses one person's data to another, so the request mechanism is
// itself a plausible attack on the deployment. Callers MUST consult this before
// assembling an export or performing an erasure.
func (r Request) MayProduceData() bool { return r.State == StateVerified }

// Verify moves a request to StateVerified.
//
// Requires both who verified and how, because an unattributable verification is
// indistinguishable from none — and the accountability question after an
// incident is "who let this through, on what evidence".
func (r *Request) Verify(by, how string, now time.Time) error {
	if r.State != StateOpen {
		return fmt.Errorf("datasubject: cannot verify a request in state %q", r.State)
	}
	if strings.TrimSpace(by) == "" || strings.TrimSpace(how) == "" {
		return fmt.Errorf("datasubject: verification requires both who verified and how " +
			"(an unattributable verification is indistinguishable from none)")
	}
	r.State, r.VerifiedBy, r.VerifiedHow, r.VerifiedAt = StateVerified, by, how, now
	return nil
}

// Action records that the right was exercised, pinning the report produced.
func (r *Request) Action(reportHash string, now time.Time) error {
	if r.State != StateVerified {
		return fmt.Errorf("datasubject: cannot action a request in state %q — "+
			"identity must be established first (Art 12(6))", r.State)
	}
	if strings.TrimSpace(reportHash) == "" {
		return fmt.Errorf("datasubject: actioning a request requires the report hash, " +
			"so what was sent remains answerable (Art 5(2))")
	}
	r.State, r.ReportHash = StateActioned, reportHash
	_ = now
	return nil
}

// Refuse records a refusal and its ground.
//
// Available from Open as well as Verified: the commonest lawful refusal is
// "we cannot identify you from this" under Art 12(6), which by definition
// happens before verification.
func (r *Request) Refuse(reason string) error {
	switch r.State {
	case StateOpen, StateVerified:
	default:
		return fmt.Errorf("datasubject: cannot refuse a request in state %q", r.State)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("datasubject: a refusal requires a stated ground — " +
			"Art 12(4) obliges telling the subject why, and an unexplained refusal is the shape of obstruction")
	}
	r.State, r.RefusedReason = StateRefused, reason
	return nil
}

// FirstMonthEnd is the end of "one month of receipt of the request" (Art
// 12(3)): the unextended deadline, and the last moment the subject may be
// told of an extension.
func FirstMonthEnd(opened time.Time) time.Time { return monthsAfter(opened, 1) }

// Extend applies the Art 12(3) extension.
//
// Only from a live state, and only with a reason and the date the subject was
// told: the extension is lawful only for complex or numerous requests AND only
// where the subject was informed, with the reasons, "within one month of
// receipt of the request" (Art 12(3)). The notice date is refused when it is
// missing, in the future, before the request, or after FirstMonthEnd.
func (r *Request) Extend(reason string, notifiedAt, now time.Time) error {
	switch r.State {
	case StateOpen, StateVerified:
	default:
		return fmt.Errorf("datasubject: cannot extend a request in state %q", r.State)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("datasubject: an Art 12(3) extension requires a reason (complexity or number of requests)")
	}
	// A timely notice may be recorded after the first month; the notice date,
	// not the recording date, is what Art 12(3) bounds.
	limit := FirstMonthEnd(r.OpenedAt)
	switch {
	case notifiedAt.IsZero():
		return fmt.Errorf("datasubject: an Art 12(3) extension needs the date the subject was told of it, " +
			"with the reasons")
	case notifiedAt.After(now):
		return fmt.Errorf("datasubject: the extension notice date %s is in the future", notifiedAt.Format(time.RFC3339))
	case notifiedAt.Before(r.OpenedAt):
		return fmt.Errorf("datasubject: the extension notice date is before the request was received")
	case notifiedAt.After(limit):
		return fmt.Errorf("datasubject: the subject must be told of the extension within one month of receipt "+
			"(by %s); a notice on %s is too late, so the extension cannot be applied retroactively to a deadline already missed",
			limit.Format(time.RFC3339), notifiedAt.Format(time.RFC3339))
	}
	r.Extended, r.ExtendedReason, r.ExtensionNotifiedAt = true, reason, notifiedAt
	return nil
}

// EscalationKey identifies one Art 12(3) escalation: a deferred row of a
// request, against the deadline it was raised for (GitHub #344 5d-1c).
type EscalationKey struct {
	RequestID string
	Table     LinkableTable
	RowID     string
	Deadline  time.Time
}

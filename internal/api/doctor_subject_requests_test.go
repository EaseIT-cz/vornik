package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #115 status audit (2026-10-21): the GDPR design §4.8 promised a doctor
// warning on a rights request nearing its Art 12(3) deadline, and on 2026-09-25
// recorded that none was built. The clock was visible only to an operator who
// opened `vornikctl subject requests` or the regulatory page. These tests pin
// the check that closes that (GDPR design §4.20).

const subjectReqSubject = "ds_doctor_subject"

// subjectLedgerFixture is a real SQLite ledger with one subject, so the check
// reads through the same repository the daemon wires.
func subjectLedgerFixture(t *testing.T) (*sql.DB, *DoctorHandlers, func(id string, kind datasubject.RequestKind, state datasubject.RequestState, opened time.Time, extended bool)) {
	t.Helper()
	db := sqlitetest.File(t, "doctor-subject-requests.db")
	ledger := sqlite.NewDataSubjectRepository(db.DB)
	ctx := context.Background()
	if err := ledger.CreateSubject(ctx, datasubject.Subject{ID: subjectReqSubject, DisplayName: "Jane Sentinel"}); err != nil {
		t.Fatalf("CreateSubject: %v", err)
	}
	h := NewDoctorHandlers(db.DB)
	h.SetSubjectRequestLedger(ledger)
	add := func(id string, kind datasubject.RequestKind, state datasubject.RequestState, opened time.Time, extended bool) {
		t.Helper()
		req := datasubject.Request{ID: id, SubjectID: subjectReqSubject, Kind: kind, State: datasubject.StateOpen, OpenedAt: opened}
		if kind == datasubject.RequestErasure {
			req.ErasureGround = datasubject.ErasureGround("consent_withdrawn")
		}
		if err := ledger.CreateRequest(ctx, req); err != nil {
			t.Fatalf("CreateRequest %s: %v", id, err)
		}
		if state != datasubject.StateOpen || extended {
			req.State = state
			req.Extended = extended
			if extended {
				req.ExtendedReason = "complex request"
			}
			if state == datasubject.StateRefused {
				req.RefusedReason = "manifestly unfounded"
			}
			if state == datasubject.StateVerified || state == datasubject.StateActioned {
				req.VerifiedBy, req.VerifiedHow, req.VerifiedAt = "op", "channel round-trip", opened
			}
			if state == datasubject.StateActioned {
				req.ReportHash = "sha256:00"
			}
			if err := ledger.SaveRequest(ctx, req); err != nil {
				t.Fatalf("SaveRequest %s: %v", id, err)
			}
		}
	}
	return db.DB, h, add
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestSubjectRequestDeadlines_OverdueIsAnErrorNamingTheRequestNotTheSubject(t *testing.T) {
	_, h, add := subjectLedgerFixture(t)
	now := time.Now().UTC()
	add("dsr_overdue", datasubject.RequestErasure, datasubject.StateVerified, now.Add(-days(31)), false)
	add("dsr_intime", datasubject.RequestAccess, datasubject.StateOpen, now.Add(-days(2)), false)

	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Name != "subject_request_deadlines" {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Status != "ERROR" {
		t.Fatalf("an overdue request must be ERROR, got %s: %s", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "1 rights request") || !strings.Contains(c.Message, "Art 12(3)") {
		t.Errorf("message must count the overdue request and cite Art 12(3): %q", c.Message)
	}
	joined := strings.Join(c.Items, "\n")
	if !strings.Contains(joined, "dsr_overdue") || !strings.Contains(joined, "erasure") {
		t.Errorf("items must name the request and its kind: %q", joined)
	}
	if strings.Contains(joined, "dsr_intime") {
		t.Errorf("an in-time request is not flagged: %q", joined)
	}
	for _, leak := range []string{subjectReqSubject, "Jane Sentinel"} {
		if strings.Contains(c.Message+joined, leak) {
			t.Errorf("the doctor report reaches support bundles; it must not name the subject (%q)", leak)
		}
	}
}

// The doctor report is embedded in support bundles through RunReportReadOnly,
// so the privacy property is pinned on that path, for both the ERROR and the
// WARNING verdicts: no subject id and no display name anywhere in the check
// (review round 1 of §4.20, finding 2).
func TestSubjectRequestDeadlines_BundlePathNeverNamesTheSubject(t *testing.T) {
	for _, tc := range []struct {
		status string
		age    time.Duration
	}{{"ERROR", days(31)}, {"WARNING", days(25)}} {
		t.Run(tc.status, func(t *testing.T) {
			_, h, add := subjectLedgerFixture(t)
			add("dsr_bundle", datasubject.RequestAccess, datasubject.StateVerified, time.Now().UTC().Add(-tc.age), false)
			var found bool
			for _, c := range h.RunReportReadOnly(context.Background()).Checks {
				if c.Name != "subject_request_deadlines" {
					continue
				}
				found = true
				if c.Status != tc.status {
					t.Fatalf("status = %s, want %s: %s", c.Status, tc.status, c.Message)
				}
				all := c.Message + "\n" + strings.Join(c.Items, "\n")
				if !strings.Contains(all, "dsr_bundle") {
					t.Errorf("the request must be named: %q", all)
				}
				for _, leak := range []string{subjectReqSubject, "Jane Sentinel"} {
					if strings.Contains(all, leak) {
						t.Errorf("the bundle path names the subject (%q): %q", leak, all)
					}
				}
			}
			if !found {
				t.Fatal("subject_request_deadlines is not in the read-only report")
			}
		})
	}
}

func TestSubjectRequestDeadlines_InsideTheWindowWarns(t *testing.T) {
	_, h, add := subjectLedgerFixture(t)
	now := time.Now().UTC()
	// Day 25 of 30: inside the 9-day warning window, not yet late.
	add("dsr_soon", datasubject.RequestAccess, datasubject.StateOpen, now.Add(-days(25)), false)

	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "WARNING" {
		t.Fatalf("a request inside the warning window must WARN, got %s: %s", c.Status, c.Message)
	}
	if !strings.Contains(strings.Join(c.Items, "\n"), "dsr_soon") {
		t.Errorf("items must name the request: %v", c.Items)
	}
}

func TestSubjectRequestDeadlines_InTimeIsOKWithTheDenominator(t *testing.T) {
	_, h, add := subjectLedgerFixture(t)
	now := time.Now().UTC()
	add("dsr_a", datasubject.RequestAccess, datasubject.StateOpen, now.Add(-days(1)), false)
	add("dsr_b", datasubject.RequestPortability, datasubject.StateVerified, now.Add(-days(3)), false)

	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "OK" {
		t.Fatalf("in-time requests are OK, got %s: %s", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "2 live rights request(s) examined") {
		t.Errorf("OK must state how many requests were examined: %q", c.Message)
	}
}

func TestSubjectRequestDeadlines_EmptyLedgerSaysSo(t *testing.T) {
	_, h, _ := subjectLedgerFixture(t)
	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "OK" {
		t.Fatalf("an empty ledger is OK, got %s: %s", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "no live rights requests") {
		t.Errorf("an empty ledger must not read as one examined and found clean: %q", c.Message)
	}
}

func TestSubjectRequestDeadlines_ExtendedUsesTheNinetyDayDeadline(t *testing.T) {
	_, h, add := subjectLedgerFixture(t)
	now := time.Now().UTC()
	// Day 40: late on the 30-day clock, in time on the extended 90-day one.
	add("dsr_ext", datasubject.RequestErasure, datasubject.StateVerified, now.Add(-days(40)), true)

	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "OK" {
		t.Fatalf("an extended request at day 40 is in time, got %s: %s", c.Status, c.Message)
	}
}

func TestSubjectRequestDeadlines_SettledRequestsAreNotCounted(t *testing.T) {
	_, h, add := subjectLedgerFixture(t)
	old := time.Now().UTC().Add(-days(60))
	add("dsr_done", datasubject.RequestAccess, datasubject.StateActioned, old, false)
	add("dsr_refused", datasubject.RequestAccess, datasubject.StateRefused, old, false)

	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "OK" || !strings.Contains(c.Message, "no live rights requests") {
		t.Fatalf("actioned and refused requests have no live clock, got %s: %s", c.Status, c.Message)
	}
}

func TestSubjectRequestDeadlines_UnwiredSkips(t *testing.T) {
	c := NewDoctorHandlers(nil).checkSubjectRequestDeadlines(context.Background())
	if c.Status != "SKIPPED" || !strings.Contains(c.Message, "not wired") {
		t.Fatalf("an unwired ledger must SKIP and say why, got %s: %s", c.Status, c.Message)
	}
}

func TestSubjectRequestDeadlines_MissingTableSkipsWithoutDriverText(t *testing.T) {
	db, h, _ := subjectLedgerFixture(t)
	if _, err := db.Exec(`DROP TABLE data_subject_requests`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	c := h.checkSubjectRequestDeadlines(context.Background())
	if c.Status != "SKIPPED" {
		t.Fatalf("a database without the ledger must SKIP, got %s: %s", c.Status, c.Message)
	}
	for _, sig := range sqlDriverErrorSignatures {
		if strings.Contains(c.Message, sig) {
			t.Errorf("a driver error is never a verdict: %q", c.Message)
		}
	}
	if len(c.Items) == 0 {
		t.Error("the raw error is kept in Items for diagnosis")
	}
}

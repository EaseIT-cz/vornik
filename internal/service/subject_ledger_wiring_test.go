package service

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// audit 2026-10-12 (GitHub #355): the daemon wired the regulatory record's
// subject-request ledger only on Postgres, so a SQLite page said the ledger did
// not exist. It is wired on the resolved driver now, SQLite included.
func TestSubjectLedgerFor_WiresTheLedgerOnSQLite(t *testing.T) {
	db := sqlitetest.File(t, "ledger.db")
	ledger := subjectLedgerFor("sqlite", db.DB)
	if ledger == nil {
		t.Fatal("the subject-request ledger must be wired on SQLite")
	}
	ctx := context.Background()
	if err := ledger.CreateSubject(ctx, datasubject.Subject{ID: "ds_wire", DisplayName: "W"}); err != nil {
		t.Fatalf("the wired ledger must work on SQLite: %v", err)
	}
	if subjectLedgerFor("sqlite", nil) != nil {
		t.Error("no database: nothing to wire")
	}
	if subjectLedgerFor("mysql", db.DB) != nil {
		t.Error("an unknown driver must not be wired")
	}
}

// GitHub #115 status audit (2026-10-21, GDPR design §4.20): the doctor reads the
// same driver-resolved ledger as the regulatory page. Wired on SQLite, the
// Art 12(3) deadline check must reach the ledger rather than SKIP.
func TestDoctorSubjectLedger_WiredOnTheResolvedDriver(t *testing.T) {
	db := sqlitetest.File(t, "doctor-ledger.db")
	find := func(dh *api.DoctorHandlers) api.DoctorCheck {
		t.Helper()
		for _, c := range dh.RunReportReadOnly(context.Background()).Checks {
			if c.Name == "subject_request_deadlines" {
				return c
			}
		}
		t.Fatal("subject_request_deadlines is not in the doctor report")
		return api.DoctorCheck{}
	}

	wired := api.NewDoctorHandlers(db.DB)
	wireDoctorSubjectLedger(wired, "sqlite", db.DB)
	if c := find(wired); c.Status != "OK" || !strings.Contains(c.Message, "no live rights requests") {
		t.Fatalf("wired on SQLite, the check must read the (empty) ledger: %s %q", c.Status, c.Message)
	}

	unknown := api.NewDoctorHandlers(db.DB)
	wireDoctorSubjectLedger(unknown, "mysql", db.DB)
	if c := find(unknown); c.Status != "SKIPPED" {
		t.Fatalf("an unknown driver leaves the check SKIPPED, got %s %q", c.Status, c.Message)
	}
}

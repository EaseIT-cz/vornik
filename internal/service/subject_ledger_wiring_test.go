package service

import (
	"context"
	"testing"

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

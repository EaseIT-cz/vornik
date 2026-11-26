package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// audit 2026-10-21 (#115, GDPR design §4.20.1 item 20; GitHub #475): on SQLite
// breach_deadlines read SKIPPED "the Art 33 incident store is Postgres-only"
// (measured 2026-11-23, §4.21.1). The ledger runs on both drivers now, so on a
// real SQLite schema the check returns a verdict.
func TestDoctor_BreachDeadlinesOnSQLite(t *testing.T) {
	db := sqlitetest.Memory(t)
	repo := sqlite.NewIncidentRepository(db.DB)
	h := NewDoctorHandlers(db.DB)
	h.SetBreachLedger(repo)
	ctx := context.Background()

	if c := h.checkBreachDeadlines(ctx); c.Status != "OK" {
		t.Fatalf("empty ledger: %s %q, want OK", c.Status, c.Message)
	}
	if err := repo.Create(ctx, incident.Incident{ID: "inc_soon", State: incident.StateDetected,
		BecameAwareAt: time.Now().UTC().Add(-60 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if c := h.checkBreachDeadlines(ctx); c.Status != "WARNING" {
		t.Fatalf("an incident 60h past awareness: %s %q, want WARNING", c.Status, c.Message)
	}
	if err := repo.Create(ctx, incident.Incident{ID: "inc_over", State: incident.StateDetected,
		BecameAwareAt: time.Now().UTC().Add(-80 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if c := h.checkBreachDeadlines(ctx); c.Status != "ERROR" {
		t.Fatalf("an incident 80h past awareness: %s %q, want ERROR", c.Status, c.Message)
	}
}

// Unwired, or on a database that predates the table, the check is SKIPPED and
// no longer blames the driver.
func TestDoctor_BreachDeadlinesSkippedWithoutALedger(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()

	if c := NewDoctorHandlers(db.DB).checkBreachDeadlines(ctx); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %s %q, want SKIPPED", c.Status, c.Message)
	}

	if _, err := db.ExecContext(ctx, `DROP TABLE security_incidents`); err != nil {
		t.Fatal(err)
	}
	h := NewDoctorHandlers(db.DB)
	h.SetBreachLedger(sqlite.NewIncidentRepository(db.DB))
	c := h.checkBreachDeadlines(ctx)
	if c.Status != "SKIPPED" || strings.Contains(c.Message, "Postgres-only") ||
		!strings.Contains(c.Message, "predates") {
		t.Fatalf("pre-ledger database: %s %q, want SKIPPED naming the missing schema, not the driver", c.Status, c.Message)
	}
}

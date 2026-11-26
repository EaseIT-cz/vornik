package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// audit 2026-10-21 (#115, GDPR design §4.20.1 item 20; GitHub #475): the Art
// 33/34 breach ledger existed only on Postgres, so on Community's default
// backend a personal-data breach could not be recorded. Same suite as the
// Postgres lane (postgres/incident_repotest_integration_test.go).

func TestIncidentLedger_Contract(t *testing.T) {
	repotest.RunIncidentLedgerSuite(t, sqlite.NewIncidentRepository(newTestDB(t).DB))
}

// A database created before the ledger reached SQLite gains the table on the
// next Migrate.
func TestIncidentLedger_ReconciledOnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.ExecContext(ctx, `DROP TABLE security_incidents`); err != nil {
		t.Fatalf("simulate an older database: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repotest.RunIncidentLedgerSuite(t, sqlite.NewIncidentRepository(db.DB))
}

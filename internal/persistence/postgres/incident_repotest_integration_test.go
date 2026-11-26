//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #475 (2026-11-23): the breach ledger on the lane a production
// deployment runs. Same suite as the SQLite file
// (sqlite/incident_repotest_test.go); a disagreement between the two is the
// finding.
func TestIncidentLedger_PostgresContract(t *testing.T) {
	repotest.RunIncidentLedgerSuite(t, NewIncidentRepository(newIntegrationDB(t).DB))
}

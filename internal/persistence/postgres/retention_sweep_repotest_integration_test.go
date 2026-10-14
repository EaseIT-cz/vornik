//go:build integration

package postgres

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #360 (2026-10-13): the retention sweeper's statements on the lane a
// production deployment runs. Same suite as the SQLite file
// (sqlite/retention_sweep_repotest_test.go); a disagreement between the two is
// the finding. Postgres binds the time.Time itself.
func TestRetentionSweep_PostgresContract(t *testing.T) {
	repotest.RunRetentionSweepSuite(t, newIntegrationDB(t).DB, func(at time.Time) any { return at })
}

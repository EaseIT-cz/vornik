package sqlite_test

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// audit 2026-10-13 (GitHub #360): the retention sweeper was Postgres-shaped —
// thresholds bound as time.Time compared wrongly on the threshold's own date,
// every global step failed at to_regclass and the artifact delete bound a
// Postgres array. The same suite runs on Postgres in the integration lane
// (postgres/retention_sweep_repotest_integration_test.go).
func TestRetentionSweep_Contract(t *testing.T) {
	repotest.RunRetentionSweepSuite(t, newTestDB(t).DB, func(at time.Time) any { return persistence.SQLiteTime(at) })
}

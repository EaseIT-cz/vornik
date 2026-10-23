//go:build integration

package cli

import (
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
)

// TestTaskCancel410_ParentCancelEndsEveryDescendant_Postgres is GitHub #410's
// found-at-level regression (task_cancel_410_test.go) on the Postgres driver:
// the same `vornikctl task cancel` scenario over the real API handler and a
// real executor, with the pgvector lane's repositories (make
// test-integration). A cascade write that differs between the drivers (the
// from-set, the last_error options, the execution supersede) fails here.
func TestTaskCancel410_ParentCancelEndsEveryDescendant_Postgres(t *testing.T) {
	db := dbcovOpen(t)
	project := fmt.Sprintf("p410-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, project)
	})
	run410(t, postgres.NewTaskRepository(db), postgres.NewExecutionRepository(db), project)
}

//go:build integration

package cli

import (
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
)

// TestTaskCancel490_ClosedApprovalNamesItsParent_Postgres is GitHub #490's
// regression (task_cancel_490_test.go) on the Postgres driver, where the
// ListApprovalsClosedSince LIKE and time comparison run on another engine.
func TestTaskCancel490_ClosedApprovalNamesItsParent_Postgres(t *testing.T) {
	db := dbcovOpen(t)
	project := fmt.Sprintf("p490-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, project)
	})
	run490(t, postgres.NewTaskRepository(db), postgres.NewExecutionRepository(db), project)
}

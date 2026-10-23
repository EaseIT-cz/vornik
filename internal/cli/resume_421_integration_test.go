//go:build integration

package cli

import (
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
)

// GitHub #421's found-at-level regression (resume_421_test.go) on the
// Postgres driver: the same REST resume scenarios over the real API handler
// and a real executor, with the pgvector lane's repositories (make
// test-integration). A conditional write that differs between the drivers
// fails here.
func TestResume421_Postgres(t *testing.T) {
	db := dbcovOpen(t)
	for _, tc := range []struct {
		name  string
		raced bool
	}{{"CancelBetweenReadAndWriteWins", true}, {"CancelledBeforeResumeStaysCancelled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			project := fmt.Sprintf("r421-%t-%d", tc.raced, time.Now().UnixNano())
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, project)
				_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, project)
			})
			run421(t, postgres.NewTaskRepository(db), postgres.NewExecutionRepository(db), project, tc.raced)
		})
	}
}

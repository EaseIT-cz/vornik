//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// #756: the home page's "Done today", same suite as the SQLite lane.
func TestExecutionList_CompletedSince_756_Postgres(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunExecutionCompletedSinceSuite(t, NewExecutionRepository(db.DB), NewTaskRepository(db.DB))
}

// #757: the Results page's jobs done by project, same suite as SQLite.
func TestExecutionCountCompletedByProject_757_Postgres(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunCountCompletedByProjectSuite(t, NewExecutionRepository(db.DB), NewTaskRepository(db.DB))
}

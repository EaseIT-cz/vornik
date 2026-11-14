package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// #756: the home page's "Done today" (ui-design-refresh-design.md §14.2).
func TestExecutionList_CompletedSince_756(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionCompletedSinceSuite(t, sqlite.NewExecutionRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

// #757: the Results page's jobs done by project (design §15.2).
func TestExecutionCountCompletedByProject_757(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCountCompletedByProjectSuite(t, sqlite.NewExecutionRepository(db.DB), sqlite.NewTaskRepository(db.DB))
}

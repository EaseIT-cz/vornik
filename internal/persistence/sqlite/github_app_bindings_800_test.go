package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #800: the binding contract on SQLite.
func TestGitHubAppBindings_Contract_800(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGitHubAppBindingSuite(t, sqlite.NewGitHubAppBindingRepository(db.DB))
}

func TestGitHubAppBindings_Race_800(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGitHubAppBindingRaceSuite(t, sqlite.NewGitHubAppBindingRepository(db.DB))
}

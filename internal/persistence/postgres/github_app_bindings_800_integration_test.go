//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #800: the binding contract on Postgres.
func TestGitHubAppBindings_PostgresContract_800(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunGitHubAppBindingSuite(t, NewGitHubAppBindingRepository(db.DB))
}

func TestGitHubAppBindings_PostgresRace_800(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunGitHubAppBindingRaceSuite(t, NewGitHubAppBindingRepository(db.DB))
}

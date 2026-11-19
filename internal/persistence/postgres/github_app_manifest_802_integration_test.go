//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #802: the App-manifest state contract on Postgres.
func TestGitHubAppManifestStates_PostgresContract_802(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunGitHubAppManifestStateSuite(t, NewGitHubAppManifestStateRepository(db.DB))
}

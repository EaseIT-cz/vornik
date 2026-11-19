package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #802: the App-manifest state contract on SQLite.
func TestGitHubAppManifestStates_Contract_802(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGitHubAppManifestStateSuite(t, sqlite.NewGitHubAppManifestStateRepository(db.DB))
}

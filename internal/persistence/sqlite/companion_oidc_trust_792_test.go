package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #792: the companion OIDC trust contract on SQLite.
func TestCompanionOIDCTrusts_Contract_792(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCompanionOIDCTrustSuite(t, sqlite.NewCompanionOIDCTrustRepository(db.DB), sqlite.NewAPIKeyRepository(db.DB))
}

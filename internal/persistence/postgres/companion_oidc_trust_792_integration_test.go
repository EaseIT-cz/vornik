//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #792: the companion OIDC trust contract on Postgres.
func TestCompanionOIDCTrusts_PostgresContract_792(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunCompanionOIDCTrustSuite(t, NewCompanionOIDCTrustRepository(db.DB), NewAPIKeyRepository(db.DB))
}

//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #355 (2026-10-13): the data-subject store, the rights ledger, the
// Art 18 record and the firewall evaluation audit, on the lane a production
// deployment runs. Same suites as the SQLite file
// (sqlite/data_subject_repotest_test.go); a disagreement between the two is the
// finding.

func TestDataSubjectStore_PostgresContract(t *testing.T) {
	repotest.RunDataSubjectStoreSuite(t, NewDataSubjectRepository(newIntegrationDB(t).DB))
}

func TestDataSubjectLedger_PostgresContract(t *testing.T) {
	repotest.RunDataSubjectLedgerSuite(t, NewDataSubjectRepository(newIntegrationDB(t).DB))
}

func TestDataSubjectRestrictionStore_PostgresContract(t *testing.T) {
	repotest.RunDataSubjectRestrictionStoreSuite(t, NewDataSubjectRepository(newIntegrationDB(t).DB))
}

func TestDataSubjectCollectErase_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunDataSubjectCollectEraseSuite(t, NewDataSubjectRepository(db.DB),
		func(ctx context.Context, id, projectID, message string) error {
			_, err := db.DB.ExecContext(ctx, `INSERT INTO chat_audit_log (id, project_id, user_message)
				VALUES ($1, $2, $3)`, id, projectID, message)
			return err
		},
		seedChunkForSuite(db))
}

func TestMemoryPolicyEvaluation_PostgresContract(t *testing.T) {
	repotest.RunMemoryPolicyEvaluationSuite(t, NewMemoryPolicyEvaluationRepository(newIntegrationDB(t).DB))
}

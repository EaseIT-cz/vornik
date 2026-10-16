//go:build integration

package postgres

import (
	"context"
	"database/sql"
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

// audit 2026-10-16 (GitHub #344, slice 5d-E): transcript exports omitted tool
// arguments, signals, metadata and the author. Same suite as SQLite.
func TestDataSubjectTranscriptExport_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunDataSubjectTranscriptExportSuite(t, NewDataSubjectRepository(db.DB), pgTranscriptSeeder{db.DB})
}

type pgTranscriptSeeder struct{ db *sql.DB }

func (s pgTranscriptSeeder) SeedChatTurnFull(ctx context.Context, id, projectID, userID, message, toolCallsJSON, signalsJSON string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_audit_log
		(id, project_id, user_id, user_message, tool_calls_json, hallucination_signals_json)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, projectID, userID, message, toolCallsJSON, signalsJSON)
	return err
}

func (s pgTranscriptSeeder) SeedTaskMessage(ctx context.Context, id, projectID, authorID, content, metadataJSON string) error {
	taskID := "task-" + id
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, project_id, status, created_at, updated_at)
		VALUES ($1, $2, 'COMPLETED', now(), now())`, taskID, projectID); err != nil {
		return err
	}
	var author, meta any
	if authorID != "" {
		author = authorID
	}
	if metadataJSON != "" {
		meta = metadataJSON
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_messages
		(id, task_id, author_kind, author_id, message_kind, content, metadata)
		VALUES ($1, $2, 'operator', $3, 'message', $4, $5::jsonb)`,
		id, taskID, author, content, meta)
	return err
}

func TestMemoryPolicyEvaluation_PostgresContract(t *testing.T) {
	repotest.RunMemoryPolicyEvaluationSuite(t, NewMemoryPolicyEvaluationRepository(newIntegrationDB(t).DB))
}

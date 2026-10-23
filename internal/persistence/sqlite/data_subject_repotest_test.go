package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// audit 2026-10-12 (GitHub #355): the SQLite driver had no data_subjects,
// data_subject_identifiers or data_subject_requests table and a stub
// memory_policy_evaluations repository, so on Community's default backend a
// data-subject request could not be recorded and the firewall audit read empty.
// Each contract below is the one Postgres runs in the integration lane.

func TestDataSubjectStore_Contract(t *testing.T) {
	repotest.RunDataSubjectStoreSuite(t, sqlite.NewDataSubjectRepository(newTestDB(t).DB))
}

func TestDataSubjectLedger_Contract(t *testing.T) {
	repotest.RunDataSubjectLedgerSuite(t, sqlite.NewDataSubjectRepository(newTestDB(t).DB))
}

func TestDataSubjectRestrictionStore_Contract(t *testing.T) {
	repotest.RunDataSubjectRestrictionStoreSuite(t, sqlite.NewDataSubjectRepository(newTestDB(t).DB))
}

func TestDataSubjectCollectErase_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunDataSubjectCollectEraseSuite(t, sqlite.NewDataSubjectRepository(db.DB),
		seedChatTurnForSuite(db), seedChunkForSuite(db))
}

// audit 2026-10-16 (GitHub #344, slice 5d-E): transcript exports omitted tool
// arguments, signals, metadata and the author. Same suite as Postgres.
func TestDataSubjectTranscriptExport_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunDataSubjectTranscriptExportSuite(t, sqlite.NewDataSubjectRepository(db.DB), sqliteTranscriptSeeder{db})
}

type sqliteTranscriptSeeder struct{ db *sqlite.DB }

func (s sqliteTranscriptSeeder) SeedChatTurnFull(ctx context.Context, id, projectID, userID, message, toolCallsJSON, signalsJSON string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_audit_log
		(id, ts, project_id, user_id, user_message, tool_calls_json, hallucination_signals_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"), projectID, userID, message, toolCallsJSON, signalsJSON)
	return err
}

func (s sqliteTranscriptSeeder) SeedTaskMessage(ctx context.Context, id, projectID, authorID, content, metadataJSON string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	taskID := "task-" + id
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, project_id, status, created_at, updated_at)
		VALUES (?, ?, 'COMPLETED', ?, ?)`, taskID, projectID, now, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_messages
		(id, task_id, author_kind, author_id, message_kind, content, metadata, created_at)
		VALUES (?, ?, 'operator', ?, 'message', ?, ?, ?)`,
		id, taskID, nullIfEmptyText(authorID), content, nullIfEmptyText(metadataJSON), now)
	return err
}

func nullIfEmptyText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestMemoryPolicyEvaluation_Contract(t *testing.T) {
	repotest.RunMemoryPolicyEvaluationSuite(t, sqlite.NewMemoryPolicyEvaluationRepository(newTestDB(t).DB))
}

// audit 2026-10-12 (GitHub #355): a database created before the subject store
// reached SQLite gains its tables and the collector's content_title column on
// the next Migrate, and its existing rows survive.
func TestDataSubjectStore_ReconciledOnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	for _, stmt := range []string{
		`DROP TABLE data_subjects`, `DROP TABLE data_subject_identifiers`,
		`DROP TABLE data_subject_requests`, `DROP TABLE memory_policy_evaluations`,
		`ALTER TABLE project_memory_chunks DROP COLUMN content_title`,
		`INSERT INTO project_memory_chunks (id, project_id, content, created_at)
		 VALUES ('c-old', 'p-old', 'old', '2026-10-01T00:00:00.000000000Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("simulate an older database (%s): %v", stmt, err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var title *string
	if err := db.QueryRowContext(ctx, `SELECT content_title FROM project_memory_chunks WHERE id = 'c-old'`).Scan(&title); err != nil {
		t.Fatalf("content_title not reconciled: %v", err)
	}
	if title != nil {
		t.Errorf("existing chunk content_title = %q, want NULL", *title)
	}
	repotest.RunDataSubjectStoreSuite(t, sqlite.NewDataSubjectRepository(db.DB))
	repotest.RunMemoryPolicyEvaluationSuite(t, sqlite.NewMemoryPolicyEvaluationRepository(db.DB))
}

// GitHub #415 item 3 (Art 17 design §15.12): an erasure evicts the assembled
// black-box trace cache. SQLite has no such table, so the same suite asserts
// the erasure completes and reports the eviction "not applicable".
func TestDataSubjectTraceCacheEviction_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunDataSubjectTraceCacheEvictionSuite(t, sqlite.NewDataSubjectRepository(db.DB),
		seedChatTurnForSuite(db), noTraceCache{})
}

type noTraceCache struct{}

func (noTraceCache) Supported() bool { return false }
func (noTraceCache) SeedAssembledTrace(context.Context, string, string, string) error {
	return nil
}
func (noTraceCache) CountAssembledTraces(context.Context, string) (int, error) { return 0, nil }

func seedChatTurnForSuite(db *sqlite.DB) repotest.SeedChatTurn {
	return func(ctx context.Context, id, projectID, message string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO chat_audit_log (id, ts, project_id, user_message)
			VALUES (?, ?, ?, ?)`, id, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"), projectID, message)
		return err
	}
}

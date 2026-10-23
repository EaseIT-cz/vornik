package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #482 (design 22, "Amendment 2026-10-21 (#482)"): the companion-chain
// predicate and the audited, recoverable verification change, on SQLite. The
// Postgres half is TestMemoryVerification_PostgresContract.
func TestMemoryVerification_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunMemoryVerificationSuite(t, sqlite.NewMemoryVerificationRepository(db.DB), memoryVerificationHarness(db.DB))
}

func memoryVerificationHarness(db *sql.DB) repotest.MemoryVerificationHarness {
	return repotest.MemoryVerificationHarness{
		Tasks: sqlite.NewTaskRepository(db),
		SeedChunk: func(ctx context.Context, c repotest.VerificationSeedChunk) error {
			_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
				(id, project_id, content, content_hash, created_at, artifact_id, task_id,
				 producer_role, validator_role, validation_status, source_name, repo_scope)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.ProjectID, c.Content, "h-"+c.ID, persistence.SQLiteTime(time.Now()), c.ArtifactID,
				nullStr(c.TaskID), nullStr(c.ProducerRole), nullStr(c.ValidatorRole), c.Status,
				nullStr(c.SourceName), nullStr(c.RepoScope))
			return err
		},
		ReadChunk: func(ctx context.Context, id string) (string, string, error) {
			var st string
			var v sql.NullString
			err := db.QueryRowContext(ctx, `SELECT validation_status, validator_role FROM project_memory_chunks WHERE id = ?`, id).Scan(&st, &v)
			return st, v.String, err
		},
		SetChunk: func(ctx context.Context, id, status, validator string) error {
			_, err := db.ExecContext(ctx, `UPDATE project_memory_chunks SET validation_status = ?, validator_role = ? WHERE id = ?`,
				status, nullStr(validator), id)
			return err
		},
		SetParent: func(ctx context.Context, taskID, parentID string) error {
			_, err := db.ExecContext(ctx, `UPDATE tasks SET parent_task_id = ? WHERE id = ?`, nullStr(parentID), taskID)
			return err
		},
		CountBackup: func(ctx context.Context, eventID string) (int, error) {
			var n int
			err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_backup WHERE event_id = ?`, eventID).Scan(&n)
			return n, err
		},
		SeedKey: func(ctx context.Context, id, project, kind string, admin bool, ns string) error {
			a := 0
			if admin {
				a = 1
			}
			_, err := db.ExecContext(ctx, `INSERT INTO api_keys (id, project_id, name, key_hash, key_prefix, created_at,
				client_kind, agent_admin, agent_namespace) VALUES (?, ?, ?, ?, 'p', ?, ?, ?, ?)`,
				id, project, repotest.VerificationKeyName(id), "h-"+id, persistence.SQLiteTime(time.Now()), nullStr(kind), a, ns)
			return err
		},
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

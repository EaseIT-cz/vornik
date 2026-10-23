//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #482 (design 22, "Amendment 2026-10-21 (#482)"): the Postgres half of
// the memory-verification contract; the SQLite half is
// TestMemoryVerification_Contract in internal/persistence/sqlite.
func TestMemoryVerification_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	raw := db.DB
	nullStr := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	repotest.RunMemoryVerificationSuite(t, NewMemoryVerificationRepository(raw), repotest.MemoryVerificationHarness{
		Tasks: NewTaskRepository(raw),
		SeedChunk: func(ctx context.Context, c repotest.VerificationSeedChunk) error {
			if _, err := raw.ExecContext(ctx, `INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path)
				VALUES ($1, $2, $3, 'OUTPUT', $4) ON CONFLICT (id) DO NOTHING`,
				c.ArtifactID, c.ProjectID, "seed", "/dev/null/"+c.ArtifactID); err != nil {
				return err
			}
			_, err := raw.ExecContext(ctx, `INSERT INTO project_memory_chunks
				(id, project_id, content, content_hash, artifact_id, task_id,
				 producer_role, validator_role, validation_status, source_name, repo_scope)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				c.ID, c.ProjectID, c.Content, "h-"+c.ID, c.ArtifactID, nullStr(c.TaskID),
				nullStr(c.ProducerRole), nullStr(c.ValidatorRole), c.Status, nullStr(c.SourceName), nullStr(c.RepoScope))
			return err
		},
		ReadChunk: func(ctx context.Context, id string) (string, string, error) {
			var st string
			var v sql.NullString
			err := raw.QueryRowContext(ctx, `SELECT validation_status, validator_role FROM project_memory_chunks WHERE id = $1`, id).Scan(&st, &v)
			return st, v.String, err
		},
		SetChunk: func(ctx context.Context, id, status, validator string) error {
			_, err := raw.ExecContext(ctx, `UPDATE project_memory_chunks SET validation_status = $1, validator_role = $2 WHERE id = $3`,
				status, nullStr(validator), id)
			return err
		},
		SetParent: func(ctx context.Context, taskID, parentID string) error {
			_, err := raw.ExecContext(ctx, `UPDATE tasks SET parent_task_id = $1 WHERE id = $2`, nullStr(parentID), taskID)
			return err
		},
		CountBackup: func(ctx context.Context, eventID string) (int, error) {
			var n int
			err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_backup WHERE event_id = $1`, eventID).Scan(&n)
			return n, err
		},
		SeedKey: func(ctx context.Context, id, project, kind string, admin bool, ns string) error {
			_, err := raw.ExecContext(ctx, `INSERT INTO api_keys (id, project_id, name, key_hash, key_prefix,
				client_kind, agent_admin, agent_namespace) VALUES ($1, $2, $3, $4, 'p', $5, $6, $7)`,
				id, project, repotest.VerificationKeyName(id), "h-"+id, nullStr(kind), admin, ns)
			return err
		},
	})
}

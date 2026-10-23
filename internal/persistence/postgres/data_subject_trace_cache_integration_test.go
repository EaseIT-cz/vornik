//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #415 item 3 (Art 17 design §15.12): an erasure left every cached
// black-box trace in place, a copy of task messages and audit rows served for
// 24 h and kept until the retention sweep (for ever with retention off). The
// suite runs `subject erase`'s executor against the real tables and checks the
// cache is empty afterwards. Same suite as SQLite, where the table does not
// exist and the eviction is "not applicable".
func TestIntegrationErasure_EvictsAssembledTraceCache(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunDataSubjectTraceCacheEvictionSuite(t, NewDataSubjectRepository(db.DB),
		func(ctx context.Context, id, projectID, message string) error {
			_, err := db.DB.ExecContext(ctx, `INSERT INTO chat_audit_log (id, project_id, user_message)
				VALUES ($1, $2, $3)`, id, projectID, message)
			return err
		},
		pgTraceCache{db.DB})
}

type pgTraceCache struct{ db *sql.DB }

func (pgTraceCache) Supported() bool { return true }

func (c pgTraceCache) SeedAssembledTrace(ctx context.Context, taskID, projectID, payloadJSON string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO blackbox_trace_cache
		(task_id, project_id, trace_digest, schema_ver, payload, assembled_at, expires_at)
		VALUES ($1, $2, 'sha256:test', 1, $3::jsonb, NOW(), NOW() + INTERVAL '24 hours')`,
		taskID, projectID, payloadJSON)
	return err
}

func (c pgTraceCache) CountAssembledTraces(ctx context.Context, taskID string) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM blackbox_trace_cache WHERE task_id = $1`, taskID).Scan(&n)
	return n, err
}

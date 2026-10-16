package sqlite

import (
	"context"
	"fmt"
	"regexp"

	"vornik.io/vornik/internal/persistence"
)

// ProjectDataCleanupRepository implements persistence.ProjectDataDeleter on
// SQLite: the archived-project wipe.
//
// It differs from the Postgres deleter in exactly the two ways the schema
// forces (GitHub #365, 2026-10-13 — the Postgres deleter, wired on SQLite,
// failed at `no such table: memory_embed_queue` and rolled back, deleting
// nothing):
//
//   - it runs persistence.ProjectDataTablesFor(false), which leaves out the
//     tables SQLite does not have;
//   - SQLite opens with foreign_keys(OFF), so a table that is a parent in
//     persistence.SQLiteCascadeGraph is deleted through
//     persistence.SQLiteDeleteCascadingTx, which applies the children Postgres
//     removes by ON DELETE CASCADE / SET NULL.
//
// There is no embedding-cache evictor: SQLite has no embedding_cache table, so
// nothing derived from the project's text is stored there. The stats say so
// (CacheEvictionNotApplicable) rather than reading as an unwired evictor.
//
// Every statement runs in one transaction; any failure rolls the whole wipe
// back and is returned, so the sweeper audits project.delete.failed and keeps
// the project for the next tick.
//
// see LLD § https://docs.vornik.io "2026-10-13 — the wipe on SQLite"
type ProjectDataCleanupRepository struct {
	db persistence.DBTX
}

// NewProjectDataCleanupRepository constructs the deleter over db — a pool, so
// the deleter can begin its own transaction.
func NewProjectDataCleanupRepository(db persistence.DBTX) *ProjectDataCleanupRepository {
	return &ProjectDataCleanupRepository{db: db}
}

var cleanupTableName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// DeleteProjectData wipes every project-scoped row of projectID.
func (r *ProjectDataCleanupRepository) DeleteProjectData(ctx context.Context, projectID string) (persistence.ProjectDataStats, error) {
	stats := persistence.ProjectDataStats{CacheEvictionNotApplicable: true}
	if projectID == "" {
		return stats, fmt.Errorf("project_cleanup: empty projectID would wipe all projects")
	}
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return stats, fmt.Errorf("project_cleanup: begin tx: %w", err)
	}
	q := r.db
	if ok {
		defer func() { _ = tx.Rollback() }()
		q = tx
	}
	for _, table := range persistence.ProjectDataTablesFor(false) {
		if !cleanupTableName.MatchString(table) {
			return stats, fmt.Errorf("project_cleanup: invalid table name %q", table)
		}
		var n int64
		if persistence.SQLiteCascadeParent(table) {
			n, err = persistence.SQLiteDeleteCascadingTx(ctx, q, table, "project_id = ?", projectID)
		} else {
			// The table's statements: a partly-wiped table (archival design
			// section J) scrubs its kept rows first, then the DELETE, whose
			// count is the one reported (n holds the last statement's count;
			// scrubbed rows are kept, not deleted, so they are not counted —
			// the same as the Postgres deleter).
			for _, stmt := range persistence.ProjectDataStatements(table, "?") {
				var res interface{ RowsAffected() (int64, error) }
				if res, err = q.ExecContext(ctx, stmt, projectID); err != nil {
					break
				}
				if n, err = res.RowsAffected(); err != nil {
					n, err = -1, nil
				}
			}
		}
		if err != nil {
			return stats, fmt.Errorf("project_cleanup: delete from %s: %w", table, err)
		}
		stats.TablesCleared++
		if n < 0 || stats.RowsDeleted < 0 {
			stats.RowsDeleted = -1
		} else {
			stats.RowsDeleted += n
		}
	}
	if ok {
		if err := tx.Commit(); err != nil {
			return stats, fmt.Errorf("project_cleanup: commit: %w", err)
		}
	}
	return stats, nil
}

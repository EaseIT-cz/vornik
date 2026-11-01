package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstanceExportRepository is the Postgres registry of owner exports. See
// persistence.InstanceExportRepository.
type InstanceExportRepository struct{ db DBTX }

// NewInstanceExportRepository constructs the repository.
func NewInstanceExportRepository(db DBTX) *InstanceExportRepository {
	return &InstanceExportRepository{db: db}
}

// RegisterExport inserts the record; a duplicate id is ErrExportExists.
func (r *InstanceExportRepository) RegisterExport(ctx context.Context, id string, createdAt time.Time) error {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO instance_exports (export_id, created_at) VALUES ($1, $2) ON CONFLICT (export_id) DO NOTHING`,
		id, createdAt.UTC())
	if err != nil {
		return mapDBError(err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return persistence.ErrExportExists
	}
	return nil
}

// ExportCreatedAt reads the record.
func (r *InstanceExportRepository) ExportCreatedAt(ctx context.Context, id string) (time.Time, bool, error) {
	var at time.Time
	err := r.db.QueryRowContext(ctx, `SELECT created_at FROM instance_exports WHERE export_id = $1`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, mapDBError(err)
	}
	return at.UTC(), true, nil
}

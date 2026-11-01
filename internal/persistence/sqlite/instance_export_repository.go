package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstanceExportRepository is the SQLite registry of owner exports. See
// persistence.InstanceExportRepository. Managed hosting runs Postgres; the
// SQLite half keeps the contract the same on both drivers.
type InstanceExportRepository struct{ db *sql.DB }

// NewInstanceExportRepository constructs the repository.
func NewInstanceExportRepository(db *sql.DB) *InstanceExportRepository {
	return &InstanceExportRepository{db: db}
}

// RegisterExport inserts the record; a duplicate id is ErrExportExists.
func (r *InstanceExportRepository) RegisterExport(ctx context.Context, id string, createdAt time.Time) error {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO instance_exports (export_id, created_at) VALUES (?, ?) ON CONFLICT (export_id) DO NOTHING`,
		id, sqliteTime(createdAt))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return persistence.ErrExportExists
	}
	return nil
}

// ExportCreatedAt reads the record.
func (r *InstanceExportRepository) ExportCreatedAt(ctx context.Context, id string) (time.Time, bool, error) {
	var at sqlTime
	err := r.db.QueryRowContext(ctx, `SELECT created_at FROM instance_exports WHERE export_id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at.Time.UTC(), true, nil
}

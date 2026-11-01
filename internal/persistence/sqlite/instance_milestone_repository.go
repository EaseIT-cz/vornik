package sqlite

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstanceMilestoneRepository is the SQLite store of the instance's
// once-only milestones. See persistence.InstanceMilestoneRepository.
type InstanceMilestoneRepository struct{ db *sql.DB }

// NewInstanceMilestoneRepository constructs the repository.
func NewInstanceMilestoneRepository(db *sql.DB) *InstanceMilestoneRepository {
	return &InstanceMilestoneRepository{db: db}
}

var _ persistence.InstanceMilestoneRepository = (*InstanceMilestoneRepository)(nil)

// MarkMilestone inserts name unless it is recorded; the first time stays.
func (r *InstanceMilestoneRepository) MarkMilestone(ctx context.Context, name string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO instance_milestones (name, at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`,
		name, sqliteTime(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1, err
}

// Milestones reads every recorded milestone.
func (r *InstanceMilestoneRepository) Milestones(ctx context.Context) (map[string]time.Time, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, at FROM instance_milestones`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at sqlTime
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = at.Time.UTC()
	}
	return out, rows.Err()
}

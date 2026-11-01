package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ShadowDecisionRepository is the SQLite store of the rule engine's shadow
// record (mirrors postgres migration 246; GitHub #598).
type ShadowDecisionRepository struct {
	db *sql.DB
}

// NewShadowDecisionRepository builds the repository.
func NewShadowDecisionRepository(db *sql.DB) *ShadowDecisionRepository {
	return &ShadowDecisionRepository{db: db}
}

var _ persistence.ShadowDecisionRepository = (*ShadowDecisionRepository)(nil)

// Record writes rows; the first write of a key wins.
func (r *ShadowDecisionRepository) Record(ctx context.Context, rows []persistence.ShadowDecision) error {
	for _, d := range rows {
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO trading_shadow_decisions
			   (execution_id, symbol, intent, project_id, observed_at, engine_status, engine_gate, model_status, model_gate, agree)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (execution_id, symbol, intent) DO NOTHING`,
			d.ExecutionID, d.Symbol, d.Intent, d.ProjectID, sqliteTime(d.ObservedAt),
			d.EngineStatus, d.EngineGate, d.ModelStatus, d.ModelGate, d.Agree); err != nil {
			return fmt.Errorf("sqlite: trading_shadow_decisions insert: %w", err)
		}
	}
	return nil
}

// Since returns a project's rows from since, oldest first.
func (r *ShadowDecisionRepository) Since(ctx context.Context, projectID string, since time.Time) ([]persistence.ShadowDecision, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT execution_id, symbol, intent, project_id, observed_at, engine_status, engine_gate, model_status, model_gate, agree
		   FROM trading_shadow_decisions WHERE project_id = ? AND observed_at >= ?
		  ORDER BY observed_at, symbol`, projectID, sqliteTime(since))
	if err != nil {
		return nil, fmt.Errorf("sqlite: trading_shadow_decisions list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ShadowDecision
	for rows.Next() {
		var (
			d  persistence.ShadowDecision
			at sqlTime
		)
		if err := rows.Scan(&d.ExecutionID, &d.Symbol, &d.Intent, &d.ProjectID, &at,
			&d.EngineStatus, &d.EngineGate, &d.ModelStatus, &d.ModelGate, &d.Agree); err != nil {
			return nil, fmt.Errorf("sqlite: trading_shadow_decisions scan: %w", err)
		}
		d.ObservedAt = at.Time.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

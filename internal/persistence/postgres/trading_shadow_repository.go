package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ShadowDecisionRepository is the Postgres store of the rule engine's shadow
// record (migration 246; GitHub #598).
type ShadowDecisionRepository struct {
	db persistence.DBTX
}

// NewShadowDecisionRepository builds the repository.
func NewShadowDecisionRepository(db persistence.DBTX) *ShadowDecisionRepository {
	return &ShadowDecisionRepository{db: db}
}

var _ persistence.ShadowDecisionRepository = (*ShadowDecisionRepository)(nil)

// Record writes rows; the first write of a key wins.
func (r *ShadowDecisionRepository) Record(ctx context.Context, rows []persistence.ShadowDecision) error {
	for _, d := range rows {
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO trading_shadow_decisions
			   (execution_id, symbol, intent, project_id, observed_at, engine_status, engine_gate, model_status, model_gate, agree)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (execution_id, symbol, intent) DO NOTHING`,
			d.ExecutionID, d.Symbol, d.Intent, d.ProjectID, d.ObservedAt.UTC(),
			d.EngineStatus, d.EngineGate, d.ModelStatus, d.ModelGate, d.Agree); err != nil {
			return fmt.Errorf("postgres: trading_shadow_decisions insert: %w", err)
		}
	}
	return nil
}

// Since returns a project's rows from since, oldest first.
func (r *ShadowDecisionRepository) Since(ctx context.Context, projectID string, since time.Time) ([]persistence.ShadowDecision, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT execution_id, symbol, intent, project_id, observed_at, engine_status, engine_gate, model_status, model_gate, agree
		   FROM trading_shadow_decisions WHERE project_id = $1 AND observed_at >= $2
		  ORDER BY observed_at, symbol`, projectID, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("postgres: trading_shadow_decisions list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ShadowDecision
	for rows.Next() {
		var d persistence.ShadowDecision
		if err := rows.Scan(&d.ExecutionID, &d.Symbol, &d.Intent, &d.ProjectID, &d.ObservedAt,
			&d.EngineStatus, &d.EngineGate, &d.ModelStatus, &d.ModelGate, &d.Agree); err != nil {
			return nil, fmt.Errorf("postgres: trading_shadow_decisions scan: %w", err)
		}
		d.ObservedAt = d.ObservedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

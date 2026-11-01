package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// FunnelRepository is the Postgres store of the trading entry funnel and its
// forward outcomes (migration 244; GitHub #596).
type FunnelRepository struct {
	db persistence.DBTX
}

// NewFunnelRepository builds the repository.
func NewFunnelRepository(db persistence.DBTX) *FunnelRepository {
	return &FunnelRepository{db: db}
}

var _ persistence.FunnelRepository = (*FunnelRepository)(nil)

// funnelOutcomeColumns are the outcome columns per horizon index. A fixed
// list, never built from input.
var funnelOutcomeColumns = [3][3]string{
	{"ret_1", "mfe_1", "mae_1"},
	{"ret_3", "mfe_3", "mae_3"},
	{"ret_5", "mfe_5", "mae_5"},
}

const funnelSelect = `SELECT execution_id, symbol, intent, task_id, project_id, step_id,
       observed_at, status, gate, gate_raw, reason, inputs, ref_close, ref_atr,
       ret_1, ret_3, ret_5, mfe_1, mfe_3, mfe_5, mae_1, mae_3, mae_5
  FROM trading_funnel_candidates`

// Record writes the candidates; the first write of a key wins.
func (r *FunnelRepository) Record(ctx context.Context, rows []persistence.FunnelCandidate) error {
	for _, c := range rows {
		var inputs any
		if len(c.Inputs) > 0 {
			inputs = string(c.Inputs)
		}
		_, err := r.db.ExecContext(ctx,
			`INSERT INTO trading_funnel_candidates
			   (execution_id, symbol, intent, task_id, project_id, step_id, observed_at,
			    status, gate, gate_raw, reason, inputs, ref_close, ref_atr)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14)
			 ON CONFLICT (execution_id, symbol, intent) DO NOTHING`,
			c.ExecutionID, c.Symbol, c.Intent, c.TaskID, c.ProjectID, c.StepID, c.ObservedAt.UTC(),
			c.Status, c.Gate, c.GateRaw, c.Reason, inputs, c.RefClose, c.RefATR)
		if err != nil {
			return fmt.Errorf("postgres: trading_funnel_candidates insert: %w", err)
		}
	}
	return nil
}

// Pending returns candidates of symbol with a reference close and an
// outcome still unknown.
func (r *FunnelRepository) Pending(ctx context.Context, projectID, symbol string) ([]persistence.FunnelCandidate, error) {
	return r.list(ctx, funnelSelect+`
	 WHERE project_id = $1 AND symbol = $2 AND ref_close IS NOT NULL
	   AND (ret_1 IS NULL OR ret_3 IS NULL OR ret_5 IS NULL)
	 ORDER BY observed_at`, projectID, symbol)
}

// Since returns a project's candidates observed at or after since.
func (r *FunnelRepository) Since(ctx context.Context, projectID string, since time.Time) ([]persistence.FunnelCandidate, error) {
	return r.list(ctx, funnelSelect+`
	 WHERE project_id = $1 AND observed_at >= $2
	 ORDER BY observed_at, symbol`, projectID, since.UTC())
}

// SetOutcome writes horizon i's outcome while it is unknown.
func (r *FunnelRepository) SetOutcome(ctx context.Context, executionID, symbol, intent string, i int, ret float64, mfe, mae *float64) error {
	if i < 0 || i >= len(funnelOutcomeColumns) {
		return fmt.Errorf("postgres: trading_funnel_candidates: no horizon %d", i)
	}
	cols := funnelOutcomeColumns[i]
	_, err := r.db.ExecContext(ctx,
		`UPDATE trading_funnel_candidates SET `+cols[0]+` = $4, `+cols[1]+` = $5, `+cols[2]+` = $6
		  WHERE execution_id = $1 AND symbol = $2 AND intent = $3 AND `+cols[0]+` IS NULL`,
		executionID, symbol, intent, ret, mfe, mae)
	if err != nil {
		return fmt.Errorf("postgres: trading_funnel_candidates outcome: %w", err)
	}
	return nil
}

func (r *FunnelRepository) list(ctx context.Context, q string, args ...any) ([]persistence.FunnelCandidate, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: trading_funnel_candidates list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.FunnelCandidate
	for rows.Next() {
		var (
			c      persistence.FunnelCandidate
			inputs sql.NullString
		)
		if err := rows.Scan(&c.ExecutionID, &c.Symbol, &c.Intent, &c.TaskID, &c.ProjectID, &c.StepID,
			&c.ObservedAt, &c.Status, &c.Gate, &c.GateRaw, &c.Reason, &inputs, &c.RefClose, &c.RefATR,
			&c.Ret[0], &c.Ret[1], &c.Ret[2], &c.MFE[0], &c.MFE[1], &c.MFE[2], &c.MAE[0], &c.MAE[1], &c.MAE[2]); err != nil {
			return nil, fmt.Errorf("postgres: trading_funnel_candidates scan: %w", err)
		}
		if inputs.Valid {
			c.Inputs = []byte(inputs.String)
		}
		c.ObservedAt = c.ObservedAt.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

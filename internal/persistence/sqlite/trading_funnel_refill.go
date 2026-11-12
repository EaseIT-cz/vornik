package sqlite

import (
	"context"
	"fmt"
	"time"
)

// funnelOutcomeRefill is Postgres migration 252 on SQLite (GitHub #719,
// trading entry-policy design, Amendment 2026-11-11). Outcomes filled before
// it measured broker bars against the scorecard's ref_close and ref_atr;
// they are written once, so they are cleared and the next pre-warm refills
// them. Run once, recorded in vornik_data_migrations (created by
// applyTimestampNormalization, which runs first).
const funnelOutcomeRefill = "2026-11-11-trading-funnel-outcomes-refill"

func (d *DB) applyFunnelOutcomeRefill(ctx context.Context) error {
	var done int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = ?`, funnelOutcomeRefill).Scan(&done); err != nil {
		return fmt.Errorf("sqlite: data migrations marker: %w", err)
	}
	if done > 0 {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: %s: %w", funnelOutcomeRefill, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE trading_funnel_candidates
   SET ret_1 = NULL, ret_3 = NULL, ret_5 = NULL,
       mfe_1 = NULL, mfe_3 = NULL, mfe_5 = NULL,
       mae_1 = NULL, mae_3 = NULL, mae_5 = NULL`); err != nil {
		return fmt.Errorf("sqlite: %s: %w", funnelOutcomeRefill, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES (?, ?)`,
		funnelOutcomeRefill, sqliteTime(time.Now())); err != nil {
		return fmt.Errorf("sqlite: record %s: %w", funnelOutcomeRefill, err)
	}
	return tx.Commit()
}

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
// them.
const funnelOutcomeRefill = "2026-11-11-trading-funnel-outcomes-refill"

// barSessionRepair is Postgres migration 259 on SQLite (GitHub #824,
// Amendment 2026-11-18): every benchmark close was stored one session early
// and every funnel outcome since #719 used a late baseline; both are cleared
// and the next pre-warm records them again.
const barSessionRepair = "2026-11-18-trading-bar-session-repair"

const clearFunnelOutcomes = `UPDATE trading_funnel_candidates
   SET ret_1 = NULL, ret_3 = NULL, ret_5 = NULL,
       mfe_1 = NULL, mfe_3 = NULL, mfe_5 = NULL,
       mae_1 = NULL, mae_3 = NULL, mae_5 = NULL`

func (d *DB) applyFunnelOutcomeRefill(ctx context.Context) error {
	return d.applyDataMigrationOnce(ctx, funnelOutcomeRefill, clearFunnelOutcomes)
}

func (d *DB) applyBarSessionRepair(ctx context.Context) error {
	return d.applyDataMigrationOnce(ctx, barSessionRepair, `DELETE FROM trading_benchmark_closes`, clearFunnelOutcomes)
}

// applyDataMigrationOnce runs stmts in one transaction unless name is
// already recorded in vornik_data_migrations (created by
// applyTimestampNormalization, which runs first), then records it.
func (d *DB) applyDataMigrationOnce(ctx context.Context, name string, stmts ...string) error {
	var done int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = ?`, name).Scan(&done); err != nil {
		return fmt.Errorf("sqlite: data migrations marker: %w", err)
	}
	if done > 0 {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES (?, ?)`,
		name, sqliteTime(time.Now())); err != nil {
		return fmt.Errorf("sqlite: record %s: %w", name, err)
	}
	return tx.Commit()
}

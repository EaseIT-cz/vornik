//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
)

// Migration 254 (ui-design-refresh-design.md §20): the instance-wide SumCost
// filters on recorded_at alone, and PostgreSQL 16 read the whole usage table
// for every window. The index must exist and lead on recorded_at.
func TestIntegrationMigration254_UsageRecordedAtIndex(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var indexdef string
	if err := db.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes
		WHERE tablename = 'task_llm_usage' AND indexname = 'idx_task_llm_usage_recorded_at'`).Scan(&indexdef); err != nil {
		t.Fatalf("idx_task_llm_usage_recorded_at: %v", err)
	}
	if !strings.Contains(indexdef, "(recorded_at)") {
		t.Fatalf("the index does not lead on recorded_at: %s", indexdef)
	}
}

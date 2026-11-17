package sqlite_test

import (
	"context"
	"strings"
	"testing"
)

// ui-design-refresh-design.md §20: the instance-wide SumCost filters on
// recorded_at alone, so the window needs an index leading on it (Postgres
// migration 254; here, schema.go). Without it each Home load read the whole
// usage table six times.
func TestSQLite_UsageRecordedAtIndex_20(t *testing.T) {
	db := newTestDB(t)
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_task_llm_usage_recorded_at' AND tbl_name = 'task_llm_usage'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("task_llm_usage has no index on recorded_at")
	}
	var detail string
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN
		SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE recorded_at >= ?`, "2026-11-15T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, notused int
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		if detail != "" {
			break
		}
	}
	if want := "idx_task_llm_usage_recorded_at"; !strings.Contains(detail, want) {
		t.Errorf("the SumCost window does not use %s: %q", want, detail)
	}
}

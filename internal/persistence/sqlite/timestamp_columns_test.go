package sqlite

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// notTimestampColumns are TEXT columns with a time-like name that are NOT
// timestamps, each with its reason. Every other TEXT column with a time-like
// name must be in the normaliser's set (SQLite timestamp ordering design, D4),
// so a new timestamp column cannot be missed silently.
var notTimestampColumns = map[string]string{
	"class_e_slot_reservations.day":                "a fixed-width YYYY-MM-DD key; orders correctly as is",
	"control_plane_proposals.blast_radius":         "text",
	"fixit_sessions.last_envelope":                 "JSON text",
	"forge_pr_review_state.last_reviewed_head_sha": "a commit sha",
	"project_ingest_queue.last_error":              "error text",
	"task_scratchpads.last_execution_id":           "an id",
	"tasks.last_error":                             "error text",
	"tasks.last_error_class":                       "a class name",
	"trading_orders.last_status_reason":            "text",
	"trading_orders.time_in_force":                 "an order attribute (DAY, GTC)",
}

var timeLikeColumn = regexp.MustCompile(`(?i)(time|date|expire|until|since|seen|deadline|due|stamp|last_|day)`)

func TestTimestampColumnsAreClassified(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	cols, err := timestampColumns(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		selected[c.table+"."+c.column] = true
	}
	for extra := range timestampExtraColumns {
		if !selected[extra] {
			t.Errorf("timestampExtraColumns names %s, which the schema does not have", extra)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT m.name, p.name, p.type FROM sqlite_master m, pragma_table_info(m.name) p
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	examined := 0
	for rows.Next() {
		var table, column, declared string
		if err := rows.Scan(&table, &column, &declared); err != nil {
			t.Fatal(err)
		}
		key := table + "." + column
		seen[key] = true
		upper := strings.ToUpper(declared)
		textual := strings.Contains(upper, "TEXT") || upper == ""
		if selected[key] || !textual || !timeLikeColumn.MatchString(column) {
			continue
		}
		examined++
		if _, ok := notTimestampColumns[key]; !ok {
			t.Errorf("%s (%s) has a time-like name but is neither normalised nor listed as not a timestamp: add it to timestampExtraColumns or notTimestampColumns", key, declared)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The denominator: a walk that examined nothing reports clean about
	// everything.
	if examined == 0 || len(selected) < 150 { // 159 on 2026-10-01
		t.Fatalf("examined %d time-like TEXT columns and selected %d timestamp columns; the schema walk is not seeing the schema", examined, len(selected))
	}
	for key := range notTimestampColumns {
		if !seen[key] {
			t.Errorf("notTimestampColumns names %s, which the schema does not have", key)
		}
	}
	t.Logf("selected %d timestamp columns; examined %d other time-like TEXT columns", len(selected), examined)
}

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// One-time normalisation of stored timestamps to sqliteTimeLayout (SQLite
// timestamp ordering design, D4). Without it a column mixes old and new forms,
// which still compares wrong. It recognises exactly three legacy shapes and
// keeps the instant; anything else is left alone and counted.

const timestampDataMigration = "2026-10-01-fixed-width-timestamps"

// timestampExtraColumns are TEXT timestamp columns neither selection rule
// catches: their declared type is plain TEXT and their name does not end in
// _at. Found by walking the migrated schema (design D4).
var timestampExtraColumns = map[string]bool{
	"agent_approval_requests.apply_lease_until": true,
	"cluster_nodes.last_seen":                   true,
	"cost_tuning_canaries.window_until":         true,
	"project_memory_chunks.event_time":          true,
}

// NormalizeStats is the per-column outcome.
type NormalizeStats struct {
	Rewritten int64 `json:"rewritten"`
	Left      int64 `json:"left"`
}

const (
	globDate      = `[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]`
	globClock     = `[0-9][0-9]:[0-9][0-9]:[0-9][0-9]`
	globCanonical = globDate + `T` + globClock + `.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z`
)

// normalizeStatements are the three shape rewrites for one column, %[1]s
// being the quoted table and %[2]s the quoted column. Each WHERE matches its
// shape exactly, so a value is rewritten by at most one statement and an
// already-canonical value by none (idempotent).
var normalizeStatements = []string{
	// RFC3339Nano with Z: "…:SSZ" or "…:SS.fZ" (1–8 fraction digits).
	`UPDATE %[1]s SET %[2]s = CASE WHEN substr(%[2]s,20,1) = 'Z'
	    THEN substr(%[2]s,1,19) || '.000000000Z'
	    ELSE substr(%[2]s,1,20) || substr(substr(%[2]s,21,length(%[2]s)-21) || '000000000',1,9) || 'Z' END
	 WHERE typeof(%[2]s) = 'text' AND %[2]s GLOB '` + globDate + `T` + globClock + `*Z' AND length(%[2]s) < 30
	   AND (length(%[2]s) = 20 OR (substr(%[2]s,20,1) = '.' AND length(%[2]s) > 21
	        AND substr(%[2]s,21,length(%[2]s)-21) NOT GLOB '*[^0-9]*'))`,
	// CURRENT_TIMESTAMP: "YYYY-MM-DD HH:MM:SS".
	`UPDATE %[1]s SET %[2]s = substr(%[2]s,1,10) || 'T' || substr(%[2]s,12,8) || '.000000000Z'
	 WHERE typeof(%[2]s) = 'text' AND %[2]s GLOB '` + globDate + ` ` + globClock + `'`,
	// A bound UTC time.Time: "YYYY-MM-DD HH:MM:SS[.f] +0000 UTC".
	`UPDATE %[1]s SET %[2]s = substr(%[2]s,1,10) || 'T' || substr(%[2]s,12,8) || '.' ||
	    substr(CASE WHEN length(%[2]s) > 29 THEN substr(%[2]s,21,length(%[2]s)-30) ELSE '' END || '000000000',1,9) || 'Z'
	 WHERE typeof(%[2]s) = 'text' AND %[2]s GLOB '` + globDate + ` ` + globClock + `* +0000 UTC'
	   AND (length(%[2]s) = 29 OR (substr(%[2]s,20,1) = '.' AND length(%[2]s) BETWEEN 31 AND 39
	        AND substr(%[2]s,21,length(%[2]s)-30) NOT GLOB '*[^0-9]*'))`,
}

type tableColumn struct{ table, column string }

// timestampColumns enumerates the columns the normaliser touches: declared
// type containing TIME or DATE, a name ending in _at, or one of
// timestampExtraColumns.
func timestampColumns(ctx context.Context, db *sql.DB) ([]tableColumn, error) {
	rows, err := db.QueryContext(ctx, `
SELECT m.name, p.name, p.type FROM sqlite_master m, pragma_table_info(m.name) p
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("enumerate columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []tableColumn
	for rows.Next() {
		var table, column, declared string
		if err := rows.Scan(&table, &column, &declared); err != nil {
			return nil, err
		}
		upper := strings.ToUpper(declared)
		if strings.Contains(upper, "TIME") || strings.Contains(upper, "DATE") ||
			strings.HasSuffix(column, "_at") || timestampExtraColumns[table+"."+column] {
			out = append(out, tableColumn{table, column})
		}
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// normalizeTimestamps rewrites every recognised legacy value, one column per
// transaction, and reports per column what it rewrote and what it left.
//
// record, when non-nil, observes each column pass (a test seam: it is how a
// test proves an empty table costs nothing).
func normalizeTimestamps(ctx context.Context, db *sql.DB, record func(tableColumn)) (map[string]NormalizeStats, error) {
	cols, err := timestampColumns(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]NormalizeStats, len(cols))
	// An empty table has nothing to rewrite. Skipping it makes the pass free
	// on a fresh database, where it ran ~159 transactions for nothing: that
	// doubled Migrate under -race and pushed the sqlite test package to CI's
	// 10-minute limit (2026.10.1 release, timestamp-ordering LLD §2).
	empty := map[string]bool{}
	for _, c := range cols {
		isEmpty, seen := empty[c.table]
		if !seen {
			var hasRow int
			if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+quoteIdent(c.table)+`)`).Scan(&hasRow); err != nil {
				return out, fmt.Errorf("probe %s: %w", c.table, err)
			}
			isEmpty = hasRow == 0
			empty[c.table] = isEmpty
		}
		if isEmpty {
			continue
		}
		if record != nil {
			record(c)
		}
		st, err := normalizeColumn(ctx, db, c)
		if err != nil {
			return out, err
		}
		if st.Rewritten > 0 || st.Left > 0 {
			out[c.table+"."+c.column] = st
		}
	}
	return out, nil
}

func normalizeColumn(ctx context.Context, db *sql.DB, c tableColumn) (NormalizeStats, error) {
	var st NormalizeStats
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	table, column := quoteIdent(c.table), quoteIdent(c.column)
	for _, stmt := range normalizeStatements {
		res, err := tx.ExecContext(ctx, fmt.Sprintf(stmt, table, column))
		if err != nil {
			return st, fmt.Errorf("normalise %s.%s: %w", c.table, c.column, err)
		}
		n, _ := res.RowsAffected()
		st.Rewritten += n
	}
	// Counted after the shape rewrites above, so what is left is what no
	// shape recognised. A new shape belongs in normalizeStatements, before
	// this count.
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %[1]s WHERE typeof(%[2]s) = 'text' AND %[2]s <> '' AND NOT (%[2]s GLOB '`+globCanonical+`')`,
		table, column)).Scan(&st.Left); err != nil {
		return st, fmt.Errorf("count %s.%s: %w", c.table, c.column, err)
	}
	return st, tx.Commit()
}

// applyTimestampNormalization runs normalizeTimestamps once per database. The
// marker row is written last and carries the per-column counts as JSON, so
// an interrupted run resumes (the rewrite is idempotent) and a completed one
// is inspectable: SELECT detail FROM vornik_data_migrations.
func (d *DB) applyTimestampNormalization(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS vornik_data_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT ''
)`); err != nil {
		return fmt.Errorf("sqlite: data migrations table: %w", err)
	}
	var done int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = ?`, timestampDataMigration).Scan(&done); err != nil {
		return fmt.Errorf("sqlite: data migrations marker: %w", err)
	}
	if done > 0 {
		return nil
	}
	stats, err := normalizeTimestamps(ctx, d.DB, d.normalizeRecorder)
	if err != nil {
		return fmt.Errorf("sqlite: %s: %w", timestampDataMigration, err)
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, map[string]any{"column": k, "rewritten": stats[k].Rewritten, "left": stats[k].Left})
	}
	detail, _ := json.Marshal(ordered)
	if _, err := d.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at, detail) VALUES (?, ?, ?)`,
		timestampDataMigration, sqliteTime(time.Now()), string(detail)); err != nil {
		return fmt.Errorf("sqlite: record %s: %w", timestampDataMigration, err)
	}
	return nil
}

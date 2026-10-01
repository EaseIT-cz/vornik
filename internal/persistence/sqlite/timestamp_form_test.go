package sqlite

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// SQLite timestamp ordering (design 2026-10-01-sqlite-timestamp-ordering).
// RFC3339Nano trims trailing zeros and its "Z" sorts above every digit, so
// "…:05Z" sorted after "…:05.5Z": lease expiry and same-second ordering could
// be wrong by up to a second.

func TestSQLiteTime_StringOrderIsTimeOrder(t *testing.T) {
	base := time.Date(2026, 10, 1, 1, 2, 5, 0, time.UTC)
	times := []time.Time{
		base.Add(123456789 * time.Nanosecond),
		base,
		base.Add(500 * time.Millisecond),
		base.Add(120 * time.Millisecond),
		base.Add(100 * time.Millisecond),
		base.Add(time.Second),
	}
	texts := make([]string, len(times))
	for i, ts := range times {
		texts[i] = sqliteTime(ts)
		if len(texts[i]) != 30 {
			t.Fatalf("sqliteTime(%v) = %q, want the fixed 30-char form", ts, texts[i])
		}
	}
	sort.Strings(texts)
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i := range times {
		if texts[i] != sqliteTime(times[i]) {
			t.Fatalf("string order differs from time order at %d: %v", i, texts)
		}
	}
}

func TestSQLiteTime_RoundTrips(t *testing.T) {
	ts := time.Date(2026, 10, 1, 1, 2, 5, 120000000, time.UTC)
	var st sqlTime
	if err := st.Scan(sqliteTime(ts)); err != nil {
		t.Fatal(err)
	}
	if !st.Time.Equal(ts) {
		t.Fatalf("round trip = %v, want %v", st.Time, ts)
	}
}

// The one-time normaliser rewrites the three legacy shapes to the fixed form,
// keeps the instant, leaves anything else alone, and is idempotent.
func TestNormalizeTimestamps(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER PRIMARY KEY, created_at TEXT, seen TIMESTAMP, day TEXT, note TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ created, seen, day, note string }{
		{"2026-10-01T01:02:05Z", "2026-10-01 01:02:05", "2026-10-01", "2026-10-01T01:02:05Z"},
		{"2026-10-01T01:02:05.5Z", "2026-10-01 01:02:05.12 +0000 UTC", "2026-10-01", "x"},
		{"2026-10-01T01:02:05.123456789Z", "2026-10-01 01:02:05 +0000 UTC", "", ""},
		{"2026-10-01T01:02:05.000000000Z", "2026-10-01 03:02:05 +0200 CEST", "", ""},
		{"not a time", "", "", ""},
	}
	for i, r := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO probe VALUES (?,?,?,?,?)`, i, r.created, r.seen, r.day, r.note); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := normalizeTimestamps(ctx, db.DB, nil)
	if err != nil {
		t.Fatalf("normalizeTimestamps: %v", err)
	}
	want := map[int][2]string{
		0: {"2026-10-01T01:02:05.000000000Z", "2026-10-01T01:02:05.000000000Z"},
		1: {"2026-10-01T01:02:05.500000000Z", "2026-10-01T01:02:05.120000000Z"},
		2: {"2026-10-01T01:02:05.123456789Z", "2026-10-01T01:02:05.000000000Z"},
		3: {"2026-10-01T01:02:05.000000000Z", "2026-10-01 03:02:05 +0200 CEST"}, // non-UTC: left, counted
		4: {"not a time", ""},
	}
	// CAST reads the stored text: for a TIMESTAMP-declared column the driver
	// would otherwise parse it into a time.Time and re-render it.
	for id, w := range want {
		var created, seen, day, note string
		if err := db.QueryRowContext(ctx, `SELECT CAST(created_at AS TEXT), CAST(seen AS TEXT), day, note FROM probe WHERE id = ?`, id).Scan(&created, &seen, &day, &note); err != nil {
			t.Fatal(err)
		}
		if created != w[0] || seen != w[1] {
			t.Fatalf("row %d = (%q, %q), want (%q, %q)", id, created, seen, w[0], w[1])
		}
		if id == 0 && (day != "2026-10-01" || note != "2026-10-01T01:02:05Z") {
			t.Fatalf("a column that is not a timestamp column was rewritten: day=%q note=%q", day, note)
		}
	}
	if stats["probe.seen"].Left != 1 || stats["probe.created_at"].Left != 1 {
		t.Fatalf("stats = %+v, want one unrecognised value left in each column", stats)
	}
	again, err := normalizeTimestamps(ctx, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	for col, s := range again {
		if s.Rewritten != 0 {
			t.Fatalf("second run rewrote %d values in %s; the normaliser must be idempotent", s.Rewritten, col)
		}
	}
}

// TestNormalizeTimestamps_SkipsEmptyTables is the regression test for the
// 2026.10.1 release CI failure: the normaliser ran one transaction per
// timestamp column (about 159) on every database, including a brand-new
// empty one. That doubled Migrate under -race (≈390 ms → ≈800 ms per call,
// measured 2026-10-01). Every sqlite test migrates a fresh database, so the
// package went from about 270 s to 594 s on CI and then hit the 10-minute
// test timeout on the release follow-up commit (run 36890700516). An empty
// table has nothing to rewrite; on a fresh database the pass must cost nothing.
func TestNormalizeTimestamps_SkipsEmptyTables(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var passes []string
	record := func(c tableColumn) { passes = append(passes, c.table+"."+c.column) }

	// The pass CI paid for: Migrate's own, on a brand-new database. Every
	// table is empty in flight, the marker table included (its row is written
	// after the pass), so it must run no column pass at all.
	db.normalizeRecorder = record
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(passes) != 0 {
		t.Fatalf("the first Migrate of a fresh database ran %d column passes %v; empty tables must be skipped", len(passes), passes)
	}

	// A re-run after Migrate sees one non-empty table, the marker's own.
	passes = nil
	if _, err := normalizeTimestamps(ctx, db.DB, record); err != nil {
		t.Fatal(err)
	}
	if len(passes) != 1 || passes[0] != "vornik_data_migrations.applied_at" {
		t.Fatalf("a re-run ran %v; want only the marker table's column", passes)
	}

	// A table with rows is still normalised, every timestamp column of it.
	if _, err := db.ExecContext(ctx, `CREATE TABLE probe2 (id INTEGER PRIMARY KEY, created_at TEXT, updated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO probe2 VALUES (1, '2026-10-01T01:02:05Z', '2026-10-01 01:02:05')`); err != nil {
		t.Fatal(err)
	}
	passes = nil
	stats, err := normalizeTimestamps(ctx, db.DB, record)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(passes)
	want := []string{"probe2.created_at", "probe2.updated_at", "vornik_data_migrations.applied_at"}
	if strings.Join(passes, ",") != strings.Join(want, ",") || stats["probe2.created_at"].Rewritten != 1 || stats["probe2.updated_at"].Rewritten != 1 {
		t.Fatalf("passes = %v, stats = %+v; want %v, probe2's two columns rewritten once each", passes, stats, want)
	}
}

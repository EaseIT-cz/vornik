package persistence

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// GitHub #361 (2026-10-13): the executor that applies the Postgres cascade
// graph on SQLite (foreign_keys OFF). These tests swap in a synthetic graph;
// the real graph is checked against both schemas by the sqlite and postgres
// packages (retention design §6.2 C4) and exercised end to end by
// repotest.RunCascadeDeleteSuite.

func withGraph(t *testing.T, g []FKEdge) {
	t.Helper()
	old := SQLiteCascadeGraph
	SQLiteCascadeGraph = g
	t.Cleanup(func() { SQLiteCascadeGraph = old })
}

func cascadeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE a (id TEXT PRIMARY KEY, k TEXT, g TEXT, parent_id TEXT)`,
		`CREATE TABLE b (id TEXT PRIMARY KEY, a_id TEXT)`,
		`CREATE TABLE c (id TEXT PRIMARY KEY, b_id TEXT)`,
		`CREATE TABLE d (id TEXT PRIMARY KEY, c_id TEXT)`,
		`CREATE TABLE e (id TEXT PRIMARY KEY, a_id TEXT)`,
		`INSERT INTO a VALUES ('a1','x','y',NULL), ('a2','x','z',NULL), ('a3','x','y','a1')`,
		`INSERT INTO b VALUES ('b1','a1'), ('b2','a2')`,
		`INSERT INTO c VALUES ('c1','b1'), ('c2','b2')`,
		`INSERT INTO d VALUES ('d1','c1'), ('d2','c2')`,
		`INSERT INTO e VALUES ('e1','a1'), ('e2','a2')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db
}

func rowCount(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

var chainGraph = []FKEdge{
	{Child: "e", Column: "a_id", Parent: "a", Action: FKSetNull},
	{Child: "b", Column: "a_id", Parent: "a", Action: FKCascade},
	{Child: "c", Column: "b_id", Parent: "b", Action: FKCascade},
	{Child: "d", Column: "c_id", Parent: "c", Action: FKCascade},
}

// A two-placeholder ? predicate reaches depth 3 with the same args: the
// predicate is embedded once per statement, never renumbered (review round 1
// F1).
func TestSQLiteDeleteCascading_TwoPlaceholdersAtDepthThree(t *testing.T) {
	withGraph(t, chainGraph)
	db := cascadeDB(t)
	n, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `k = ? AND g = ?`, "x", "y")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // a1, a3
		t.Errorf("deleted %d parents, want 2", n)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM a WHERE id = 'a2'`:                  1,
		`SELECT COUNT(*) FROM b`:                                  1,
		`SELECT COUNT(*) FROM b WHERE id = 'b2'`:                  1,
		`SELECT COUNT(*) FROM c WHERE id = 'c2'`:                  1,
		`SELECT COUNT(*) FROM c`:                                  1,
		`SELECT COUNT(*) FROM d`:                                  1,
		`SELECT COUNT(*) FROM e`:                                  2,
		`SELECT COUNT(*) FROM e WHERE id = 'e1' AND a_id IS NULL`: 1,
		`SELECT COUNT(*) FROM e WHERE id = 'e2' AND a_id = 'a2'`:  1,
	} {
		if got := rowCount(t, db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// The same with $N placeholders, the retention sweeper's style.
func TestSQLiteDeleteCascading_DollarPlaceholders(t *testing.T) {
	withGraph(t, chainGraph)
	db := cascadeDB(t)
	if _, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `g = $2 AND k = $1`, "x", "z"); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM d WHERE id = 'd2'`); got != 0 {
		t.Errorf("grandchild of a2 survived")
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM d WHERE id = 'd1'`); got != 1 {
		t.Errorf("grandchild of a1 deleted")
	}
}

// A failing child statement leaves the parent and every child in place: the
// statements of one parent delete commit together or not at all.
func TestSQLiteDeleteCascadingTx_AtomicOnFailure(t *testing.T) {
	withGraph(t, append(append([]FKEdge{}, chainGraph...),
		FKEdge{Child: "missing_table", Column: "a_id", Parent: "a", Action: FKCascade}))
	db := cascadeDB(t)
	_, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `id = ?`, "a1")
	if err == nil || !strings.Contains(err.Error(), "missing_table") {
		t.Fatalf("err = %v, want the missing_table failure", err)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM a`: 3, `SELECT COUNT(*) FROM b`: 2, `SELECT COUNT(*) FROM c`: 2,
		`SELECT COUNT(*) FROM d`: 2, `SELECT COUNT(*) FROM e WHERE a_id = 'a1'`: 1,
	} {
		if got := rowCount(t, db, q); got != want {
			t.Errorf("%s = %d, want %d (rolled back)", q, got, want)
		}
	}
}

// The shipped graph has no cascade cycle; the guard is defensive and is
// exercised with a synthetic CASCADE self-edge (review round 1 F6).
func TestSQLiteDeleteCascading_RefusesCascadeCycle(t *testing.T) {
	withGraph(t, []FKEdge{{Child: "a", Column: "parent_id", Parent: "a", Action: FKCascade}})
	db := cascadeDB(t)
	if _, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `id = ?`, "a1"); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle refusal", err)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM a`); got != 3 {
		t.Errorf("a has %d rows, want 3", got)
	}
}

// A predicate that reads a column the cascade nulls on the parent itself would
// select a different set at the final delete; it is refused (design §6.2 C2).
func TestSQLiteDeleteCascading_RefusesPredicateOnSelfNulledColumn(t *testing.T) {
	withGraph(t, []FKEdge{{Child: "a", Column: "parent_id", Parent: "a", Action: FKSetNull}})
	db := cascadeDB(t)
	if _, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `id = ? OR parent_id = ?`, "a1", "a1"); err == nil ||
		!strings.Contains(err.Error(), "parent_id") {
		t.Fatalf("err = %v, want a refusal naming parent_id", err)
	}
	// id-only predicates are fine, and the self-edge is nulled.
	if _, err := SQLiteDeleteCascadingTx(context.Background(), db, "a", `id = ?`, "a1"); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM a WHERE id = 'a3' AND parent_id IS NULL`); got != 1 {
		t.Errorf("a3.parent_id not nulled")
	}
}

func TestSQLiteDeleteCascading_RejectsBadIdentifiers(t *testing.T) {
	withGraph(t, []FKEdge{{Child: "b; DROP TABLE a", Column: "a_id", Parent: "a", Action: FKCascade}})
	db := cascadeDB(t)
	if _, err := sqliteDeleteCascading(context.Background(), db, "a", `id = ?`, "a1"); err == nil {
		t.Fatal("bad child identifier accepted")
	}
	if _, err := sqliteDeleteCascading(context.Background(), db, "a b", `id = ?`, "a1"); err == nil {
		t.Fatal("bad parent identifier accepted")
	}
}

func TestSQLiteCascadeParent(t *testing.T) {
	for table, want := range map[string]bool{
		"tasks": true, "executions": true, "artifacts": true,
		"task_messages": false, "tool_audit_log": false, "task_scratchpad": false,
	} {
		if got := SQLiteCascadeParent(table); got != want {
			t.Errorf("SQLiteCascadeParent(%q) = %v, want %v", table, got, want)
		}
	}
	if FKCascade.String() != "CASCADE" || FKSetNull.String() != "SET NULL" || FKAction(0).String() != "FKAction(0)" {
		t.Error("FKAction.String")
	}
}

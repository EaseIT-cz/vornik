package sqlite_test

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #361 (2026-10-13): SQLite opens with foreign_keys(OFF), so deleting a
// task, an execution or an artifact — TaskRepository.Delete, the artifact
// deletes, retention steps 3/4/5 — left every child row behind. The same suite
// runs on Postgres in the integration lane
// (postgres/cascade_delete_integration_test.go), where the FKs do it natively.
func TestCascadeDelete_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunCascadeDeleteSuite(t, db.DB, repotest.CascadeDeleteRepos{
		Tasks:      sqlite.NewTaskRepository(db.DB),
		Executions: sqlite.NewExecutionRepository(db.DB),
		Artifacts:  sqlite.NewArtifactRepository(db.DB),
	}, func(at time.Time) any { return persistence.SQLiteTime(at) }, false)
}

// sqliteParentsNeverDeleted are parents the SQLite schema gives cascading
// children but which no SQLite code path deletes (retention design §6.1,
// denominator row 10: zero DELETE statements outside the Postgres package).
// Adding a delete of one of these means adding its edges to
// persistence.SQLiteCascadeGraph and removing it here.
var sqliteParentsNeverDeleted = map[string]bool{"instincts": true, "users": true, "groups": true}

// TestSQLiteCascadeGraph_MatchesSchema is the SQLite half of the drift check
// (retention design §6.2 C4a): the hand-written graph and the migrated schema
// must agree, so a new child table cannot be added without its delete.
func TestSQLiteCascadeGraph_MatchesSchema(t *testing.T) {
	db := newTestDB(t)
	type key struct{ child, col, parent string }
	inGraph := map[key]persistence.FKEdge{}
	seenCol := map[string]bool{}
	for _, e := range persistence.SQLiteCascadeGraph {
		inGraph[key{e.Child, e.Column, e.Parent}] = e
		// One action per column (design §6.2 C2): a column both nulled and
		// used to select a cascade would make list order matter.
		if seenCol[e.Child+"."+e.Column] {
			t.Errorf("%s.%s appears twice in the graph", e.Child, e.Column)
		}
		seenCol[e.Child+"."+e.Column] = true
	}

	// (1) Every declared CASCADE / SET NULL edge is in the graph.
	rows, err := db.Query(`SELECT m.name, f."from", f."table", f.on_delete
		FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f.on_delete IN ('CASCADE', 'SET NULL')`)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for rows.Next() {
		var child, col, parent, action string
		if err := rows.Scan(&child, &col, &parent, &action); err != nil {
			t.Fatal(err)
		}
		declared++
		if sqliteParentsNeverDeleted[parent] {
			continue
		}
		e, ok := inGraph[key{child, col, parent}]
		if !ok {
			t.Errorf("SQLite declares %s.%s -> %s ON DELETE %s, but persistence.SQLiteCascadeGraph has no such edge",
				child, col, parent, action)
			continue
		}
		if e.Action.String() != action {
			t.Errorf("%s.%s -> %s: schema says %s, graph says %s", child, col, parent, action, e.Action)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if declared == 0 {
		t.Fatal("no FK declarations read; the probe is broken")
	}

	// (2) Every non-PostgresOnly edge's table and column exist, and a SET NULL
	// column is nullable; every PostgresOnly column is absent.
	for _, e := range persistence.SQLiteCascadeGraph {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, e.Child).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if e.PostgresOnly {
			var col int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, e.Child, e.Column).Scan(&col); err != nil {
				t.Fatal(err)
			}
			if col != 0 {
				t.Errorf("%s.%s exists on SQLite now: clear PostgresOnly on the edge", e.Child, e.Column)
			}
			continue
		}
		if exists == 0 {
			t.Errorf("%s.%s: table missing on SQLite (mark the edge PostgresOnly?)", e.Child, e.Column)
			continue
		}
		var n, notNull int
		if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX("notnull"), 0) FROM pragma_table_info(?) WHERE name = ?`,
			e.Child, e.Column).Scan(&n, &notNull); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s.%s: column missing on SQLite", e.Child, e.Column)
		}
		if e.Action == persistence.FKSetNull && notNull == 1 {
			t.Errorf("%s.%s is NOT NULL on SQLite; a SET NULL edge would fail every delete", e.Child, e.Column)
		}
		if e.Action == persistence.FKCascade {
			// A cascade child that is itself a parent is selected by id.
			for _, c := range persistence.SQLiteCascadeGraph {
				if c.Parent == e.Child {
					var idCol int
					_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'id'`, e.Child).Scan(&idCol)
					if idCol != 1 {
						t.Errorf("%s is a parent in the graph but has no id column", e.Child)
					}
					break
				}
			}
		}
	}
}

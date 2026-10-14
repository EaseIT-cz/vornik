//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #361 (2026-10-13): the Postgres side of the cascade suite. Postgres
// applies ON DELETE CASCADE / SET NULL natively; the same suite on SQLite
// (sqlite/cascade_delete_test.go) pins that the emulated graph ends in the
// same state. A disagreement between the two lanes is the finding.
func TestCascadeDelete_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunCascadeDeleteSuite(t, db.DB, repotest.CascadeDeleteRepos{
		Tasks:      NewTaskRepository(db.DB),
		Executions: NewExecutionRepository(db.DB),
		Artifacts:  NewArtifactRepository(db.DB),
	}, func(at time.Time) any { return at }, true)
}

// TestSQLiteCascadeGraph_MatchesPostgresCatalog is the Postgres half of the
// drift check (retention design §6.2 C4b): every CASCADE / SET NULL foreign
// key Postgres has to a parent in persistence.SQLiteCascadeGraph is in the
// graph with the same action, and every graph edge is such a key. A new
// child table of tasks, executions or artifacts without a graph entry fails
// here.
func TestSQLiteCascadeGraph_MatchesPostgresCatalog(t *testing.T) {
	db := newIntegrationDB(t)
	type key struct{ child, col, parent string }
	parents := map[string]bool{}
	graph := map[key]persistence.FKEdge{}
	for _, e := range persistence.SQLiteCascadeGraph {
		parents[e.Parent] = true
		graph[key{e.Child, e.Column, e.Parent}] = e
	}
	rows, err := db.DB.QueryContext(context.Background(), `
		SELECT cl.relname, a.attname, pcl.relname, c.confdeltype
		FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = current_schema()
		JOIN pg_class pcl ON pcl.oid = c.confrelid
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND array_length(c.conkey, 1) = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[key]bool{}
	for rows.Next() {
		var child, col, parent, action string
		if err := rows.Scan(&child, &col, &parent, &action); err != nil {
			t.Fatal(err)
		}
		if !parents[parent] || (action != "c" && action != "n") {
			continue
		}
		k := key{child, col, parent}
		seen[k] = true
		e, ok := graph[k]
		if !ok {
			t.Errorf("Postgres has %s.%s -> %s ON DELETE %s; persistence.SQLiteCascadeGraph has no such edge",
				child, col, parent, map[string]string{"c": "CASCADE", "n": "SET NULL"}[action])
			continue
		}
		want := map[persistence.FKAction]string{persistence.FKCascade: "c", persistence.FKSetNull: "n"}[e.Action]
		if action != want {
			t.Errorf("%s.%s -> %s: Postgres action %q, graph %s", child, col, parent, action, e.Action)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for k := range graph {
		if !seen[k] {
			t.Errorf("graph edge %s.%s -> %s is not a CASCADE / SET NULL foreign key on Postgres", k.child, k.col, k.parent)
		}
	}
}

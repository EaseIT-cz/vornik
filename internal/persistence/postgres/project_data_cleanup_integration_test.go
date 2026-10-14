//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #365 (2026-10-13): the Postgres side of the project-wipe suite, with
// the deleter as service.initArchiveSweeper wires it (cache evictor included).
// The SQLite side is sqlite/project_data_cleanup_test.go.
func TestProjectDataWipe_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunProjectDataWipeSuite(t, db.DB, NewProjectDataCleanupRepository(db.DB, memory.EvictProjectEmbeddingCache), true)
}

// TestProjectDataTables_MatchPostgresCatalog is the Postgres guard of the
// wipe's denominator (archival LLD 2026-10-13 C5): every base table with a
// project_id column is wiped or excluded with a reason, and every listed table
// exists. agent_admin_refusals and agent_connector_installs were in neither
// (audit finding, GitHub #365).
func TestProjectDataTables_MatchPostgresCatalog(t *testing.T) {
	db := newIntegrationDB(t)
	scoped := repotest.ProjectScopedTables(t, db.DB, true)
	listed := map[string]bool{}
	for _, tb := range persistence.ProjectDataTables {
		listed[tb] = true
		// GitHub #370: a SQLiteOnly table (memory_quarantine) is skipped by the
		// Postgres wipe; if Postgres ever grows it, the flag must go.
		switch has := repotest.TableExists(t, db.DB, true, tb); {
		case !has && !persistence.ProjectDataTablesSQLiteOnly[tb]:
			t.Errorf("ProjectDataTables lists %s but Postgres has no such table", tb)
		case has && persistence.ProjectDataTablesSQLiteOnly[tb]:
			t.Errorf("%s is flagged SQLiteOnly but Postgres now has it: clear the flag so the Postgres wipe deletes it", tb)
		}
		if _, both := persistence.ProjectScopedTablesNotWiped[tb]; both {
			t.Errorf("%s is both wiped and excluded", tb)
		}
	}
	wiped, excluded := 0, 0
	for _, tb := range scoped {
		_, ex := persistence.ProjectScopedTablesNotWiped[tb]
		switch {
		case listed[tb]:
			wiped++
		case ex:
			excluded++
		default:
			t.Errorf("%s has a project_id column but is neither wiped (persistence.ProjectDataTables) nor excluded with a reason (persistence.ProjectScopedTablesNotWiped)", tb)
		}
	}
	t.Logf("project_id tables on Postgres: %d found; %d wiped, %d excluded with a reason, %d unaccounted",
		len(scoped), wiped, excluded, len(scoped)-wiped-excluded)
}

// TestProjectDataTables_CascadeChildrenCovered: every ON DELETE CASCADE / SET
// NULL key whose parent the wipe deletes has a child the wipe lists itself or
// a parent persistence.SQLiteCascadeGraph emulates — otherwise the SQLite wipe
// (foreign_keys OFF) would orphan that child (archival LLD 2026-10-13 C5).
func TestProjectDataTables_CascadeChildrenCovered(t *testing.T) {
	db := newIntegrationDB(t)
	listed := map[string]bool{}
	for _, tb := range persistence.ProjectDataTables {
		listed[tb] = true
	}
	graphParent := map[string]bool{}
	for _, e := range persistence.SQLiteCascadeGraph {
		graphParent[e.Parent] = true
	}
	rows, err := db.DB.QueryContext(context.Background(), `
		SELECT cl.relname, a.attname, pcl.relname
		FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = current_schema()
		JOIN pg_class pcl ON pcl.oid = c.confrelid
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.confdeltype IN ('c', 'n')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	checked := 0
	for rows.Next() {
		var child, col, parent string
		if err := rows.Scan(&child, &col, &parent); err != nil {
			t.Fatal(err)
		}
		// A parent SQLite does not have is never deleted there; Postgres
		// cascades its children natively.
		if !listed[parent] || persistence.ProjectDataTablesPostgresOnly[parent] {
			continue
		}
		checked++
		if !listed[child] && !graphParent[parent] {
			t.Errorf("%s.%s -> %s cascades on Postgres, but the wipe does not list %s and SQLiteCascadeGraph has no %s parent: the SQLite wipe would orphan it",
				child, col, parent, child, parent)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("cascading foreign keys into wiped tables: %d checked", checked)
}

package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #365 (2026-10-13): wiping an archived project on SQLite failed at
// `no such table: memory_embed_queue` and rolled back, deleting nothing (the
// Postgres deleter was wired on whatever c.DB was). The same suite runs on
// Postgres in the integration lane
// (postgres/project_data_cleanup_integration_test.go).
func TestProjectDataWipe_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunProjectDataWipeSuite(t, db.DB, sqlite.NewProjectDataCleanupRepository(db.DB), false)
}

// TestProjectDataTables_MatchSQLiteSchema is the SQLite guard of the wipe's
// denominator (archival LLD 2026-10-13 C5): every table with a project_id
// column is either wiped (persistence.ProjectDataTables) or excluded with a
// reason (persistence.ProjectScopedTablesNotWiped); every listed table exists
// unless flagged PostgresOnly, and a PostgresOnly one does not.
//
// GitHub #365: agent_admin_refusals and agent_connector_installs landed in
// neither and outlived every deleted project.
func TestProjectDataTables_MatchSQLiteSchema(t *testing.T) {
	db := newTestDB(t)
	checkProjectDataDenominator(t, repotest.ProjectScopedTables(t, db.DB, false), func(tb string) bool {
		return repotest.TableExists(t, db.DB, false, tb)
	}, false)
}

// TestProjectDataWipe_NoEmbeddingCacheOnSQLite pins the premise of
// ProjectDataStats.CacheEvictionNotApplicable: if SQLite ever grows an
// embedding_cache, the SQLite deleter must start evicting it.
func TestProjectDataWipe_NoEmbeddingCacheOnSQLite(t *testing.T) {
	db := newTestDB(t)
	if repotest.TableExists(t, db.DB, false, "embedding_cache") {
		t.Fatal("SQLite now has embedding_cache: the SQLite project wipe must evict it and stop reporting CacheEvictionNotApplicable")
	}
	st, err := sqlite.NewProjectDataCleanupRepository(db.DB).DeleteProjectData(context.Background(), "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if !st.CacheEvictionNotApplicable || st.CacheEvictionRan {
		t.Errorf("stats = %+v, want CacheEvictionNotApplicable and not CacheEvictionRan", st)
	}
	if want := len(persistence.ProjectDataTablesFor(false)); st.TablesCleared != want {
		t.Errorf("TablesCleared = %d, want %d", st.TablesCleared, want)
	}
}

func TestProjectDataWipe_RefusesEmptyProject(t *testing.T) {
	db := newTestDB(t)
	if _, err := sqlite.NewProjectDataCleanupRepository(db.DB).DeleteProjectData(context.Background(), ""); err == nil {
		t.Fatal("empty project id must be refused: it would wipe every project")
	}
}

// A failing statement rolls the whole wipe back: nothing of the project is
// deleted, and the error is returned rather than reported as a wipe.
func TestProjectDataWipe_FailureRollsBackAndIsReturned(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO trading_safety_events (id, project_id, kind, recorded_at)
		VALUES ('e1', 'p1', 'k', '2026-10-13T00:00:00Z')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Break a later table of the list so the wipe fails after deleting from
	// trading_safety_events.
	if _, err := db.ExecContext(ctx, `DROP TABLE api_keys`); err != nil {
		t.Fatal(err)
	}
	_, err := sqlite.NewProjectDataCleanupRepository(db.DB).DeleteProjectData(ctx, "p1")
	if err == nil || !strings.Contains(err.Error(), "api_keys") {
		t.Fatalf("err = %v, want a failure naming api_keys", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM trading_safety_events WHERE project_id = 'p1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("trading_safety_events rows = %d after a failed wipe, want 1 (rolled back)", n)
	}
}

// checkProjectDataDenominator is shared by the SQLite guard; the Postgres
// guard (integration lane) applies the same rules.
func checkProjectDataDenominator(t *testing.T, scoped []string, exists func(string) bool, postgres bool) {
	t.Helper()
	listed := map[string]bool{}
	for _, tb := range persistence.ProjectDataTables {
		if listed[tb] {
			t.Errorf("%s appears twice in ProjectDataTables", tb)
		}
		listed[tb] = true
		if _, both := persistence.ProjectScopedTablesNotWiped[tb]; both {
			t.Errorf("%s is both wiped and excluded", tb)
		}
		pgOnly := persistence.ProjectDataTablesPostgresOnly[tb]
		switch has := exists(tb); {
		case !has && (postgres || !pgOnly):
			t.Errorf("ProjectDataTables lists %s but this driver has no such table", tb)
		case has && !postgres && pgOnly:
			t.Errorf("%s is flagged PostgresOnly but SQLite now has it: clear the flag so the SQLite wipe deletes it", tb)
		}
	}
	for tb := range persistence.ProjectDataTablesPostgresOnly {
		if !listed[tb] {
			t.Errorf("ProjectDataTablesPostgresOnly names %s, which ProjectDataTables does not list", tb)
		}
	}
	// GitHub #370: a SQLiteOnly table must be listed, and must exist here (the
	// switch above already fails a listed table this driver lacks).
	for tb := range persistence.ProjectDataTablesSQLiteOnly {
		if !listed[tb] {
			t.Errorf("ProjectDataTablesSQLiteOnly names %s, which ProjectDataTables does not list", tb)
		}
	}
	// An exclusion excused as "removed with its parent" must, on SQLite
	// (foreign_keys OFF), have a SQLiteCascadeGraph edge from that parent —
	// unless the table does not exist here at all (review round 1, F2).
	for tb, reason := range persistence.ProjectScopedTablesNotWiped {
		parent, ok := strings.CutPrefix(reason, persistence.NotWipedCascadePrefix)
		if !ok || postgres || !exists(tb) {
			continue
		}
		covered := false
		for _, e := range persistence.SQLiteCascadeGraph {
			covered = covered || (e.Parent == parent && e.Child == tb && !e.PostgresOnly)
		}
		if !covered {
			t.Errorf("%s is excused as removed with %s, but SQLiteCascadeGraph has no %s -> %s edge: the SQLite wipe would orphan it", tb, parent, parent, tb)
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
	t.Logf("project_id tables on this driver: %d found; %d wiped, %d excluded with a reason, %d unaccounted",
		len(scoped), wiped, excluded, len(scoped)-wiped-excluded)
}

// TestProjectDataWipe_SuiteLeavesNoRows: the wipe suite seeds a victim and a
// control project and must remove both when it ends. Incident 2026-10-14: the
// control project's rows were never removed, so every Postgres integration run
// left one in every project table of the shared integration database. Ten
// leftover dispatcher_reminders rows in 'firing' then filled
// TestReclaimStuckFiring_ReturnsOldFiringRow's LIMIT 10 page, and the gate
// stamp on 4eb146337 went red.
func TestProjectDataWipe_SuiteLeavesNoRows(t *testing.T) {
	db := newTestDB(t)
	t.Run("suite", func(t *testing.T) {
		repotest.RunProjectDataWipeSuite(t, db.DB, sqlite.NewProjectDataCleanupRepository(db.DB), false)
	})
	rows, err := db.QueryContext(context.Background(),
		`SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) c
		 WHERE m.type = 'table' AND c.name = 'project_id'`)
	if err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	if len(tables) == 0 {
		t.Fatal("no table with a project_id column: the check examined nothing")
	}
	for _, tb := range tables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM "` + tb + `" WHERE project_id LIKE 'wipe-%'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tb, err)
		}
		if n != 0 {
			t.Errorf("%s: %d row(s) of a wipe-suite project remain after the suite", tb, n)
		}
	}
	t.Logf("examined %d tables with a project_id column", len(tables))
}

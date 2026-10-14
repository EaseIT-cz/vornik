//go:build integration

package cli

// Found-at-level regressions for the two #334 retention-window fixes
// (audit 2026-10-14; ENGINEERING-TENETS §4, found-at-level retro
// docs/audits/2026-10-14-found-at-level-retro.md).
//
// Both defects were found by audit, but an operator observes them as
// "I configured a window, ran a real sweep, and rows the configuration
// said to keep were deleted". The fixes shipped with tests on the policy
// builder (TestResolve_CarriesGraphQuarantineDays) and on a hand-built
// policy over hand-made SQLite tables
// (TestRetentionCLI_HonoursConfiguredEvictionAuditWindow); neither reads
// a configuration file or runs the operator's command on the database
// the tables live in. These drive `vornikctl retention --apply` itself:
// a config.yaml with a `retention:` block, config.Load, the project
// registry, the sweeper, real Postgres rows.
//
// Postgres only, and that is the whole population: the SQLite knowledge
// graph has no quarantine columns (the purge probes for them and refuses
// if they ever appear), and memory_eviction_audit / memory_eviction_runs
// are Postgres-schema tables.

import (
	"database/sql"
	"fmt"
	"testing"
)

// runRetentionApplyWith writes a configuration with the given retention
// block and a one-project registry, then runs `vornikctl retention
// --project <p> --apply`.
func runRetentionApplyWith(t *testing.T, project, retentionYAML string) {
	t.Helper()
	dbcovWriteConfigWith(t, dbcovDBConfig(t), retentionYAML)
	retention_reset()
	retentionWriteConfigs(t, project)
	retentionProject = project
	retentionApply = true
	t.Cleanup(retention_reset)
	out, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err != nil {
		t.Fatalf("vornikctl retention --apply: %v\n%s", err, out)
	}
}

// seedParkedEntity inserts a knowledge-graph entity parked by retention
// ageDays ago, with no mention and no edge, so nothing but the
// quarantine horizon keeps it.
func seedParkedEntity(t *testing.T, db *sql.DB, project string, ageDays int) string {
	t.Helper()
	id := fmt.Sprintf("parked-%dd-%s", ageDays, project)
	if _, err := db.Exec(`
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, lifecycle_state, quarantined_at)
		VALUES ($1, $2, 'PERSON', $1, 'quarantined', now() - make_interval(days => $3))`,
		id, project, ageDays); err != nil {
		t.Fatalf("seed parked entity: %v", err)
	}
	return id
}

func rowExists(t *testing.T, db *sql.DB, table, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n == 1
}

// #334 audit 2026-10-14 (c412d4fa7): retention.Resolve dropped
// GraphQuarantineDays, so every sweep, the daemon's and `vornikctl
// retention`, purged parked graph rows on the 30-day default. The
// documented legal hold `retention.graph_quarantine_days: -1` did
// nothing, and neither did a longer window. A row parked 40 days ago must
// survive a real sweep under -1 and under 45; the unset case (default 30)
// is the control that the sweep reached the purge at all, and the 50-day
// row under 45 shows 45 is applied as a window, not as "disabled".
func TestIntegration_Retention_GraphQuarantineLegalHoldSurvivesApply(t *testing.T) {
	db := dbcovOpen(t)
	cases := []struct {
		name      string
		yaml      string
		ages      []int
		wantAlive map[int]bool
	}{
		{"legal hold -1", "retention:\n  graph_quarantine_days: -1\n", []int{40, 400}, map[int]bool{40: true, 400: true}},
		{"window 45", "retention:\n  graph_quarantine_days: 45\n", []int{40, 50}, map[int]bool{40: true, 50: false}},
		{"unset (default 30), control", "", []int{40}, map[int]bool{40: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := dbcovUniqueProject("ret-graphhold")
			dbcovCleanupProject(t, db, project)
			ids := map[int]string{}
			for _, age := range tc.ages {
				ids[age] = seedParkedEntity(t, db, project, age)
			}
			runRetentionApplyWith(t, project, tc.yaml)
			for _, age := range tc.ages {
				alive := rowExists(t, db, "knowledge_entities", ids[age])
				if alive != tc.wantAlive[age] {
					t.Errorf("%s: entity parked %d days ago alive=%v after `vornikctl retention --apply`, want %v",
						tc.name, age, alive, tc.wantAlive[age])
				}
			}
		})
	}
}

// #334 audit 2026-10-14 (9eee76275): `vornikctl retention` built its
// policy without MemoryEvictionAuditDays, so --apply swept the
// hard-eviction tombstones on the compiled 365-day default while the
// configuration said 730. A 400-day-old tombstone and run header must
// survive; the 800-day-old pair is the control that the step ran.
func TestIntegration_Retention_EvictionAuditWindowSurvivesApply(t *testing.T) {
	db := dbcovOpen(t)
	project := dbcovUniqueProject("ret-evict")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM memory_eviction_audit WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM memory_eviction_runs WHERE project_id = $1`, project)
	})
	ids := map[int][2]string{}
	for _, age := range []int{400, 800} {
		run := fmt.Sprintf("run-%dd-%s", age, project)
		tomb := fmt.Sprintf("tomb-%dd-%s", age, project)
		if _, err := db.Exec(`INSERT INTO memory_eviction_runs (id, project_id, evicted_at)
			VALUES ($1, $2, now() - make_interval(days => $3))`, run, project, age); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO memory_eviction_audit (id, project_id, chunk_id, evicted_at)
			VALUES ($1, $2, 'c', now() - make_interval(days => $3))`, tomb, project, age); err != nil {
			t.Fatalf("seed tombstone: %v", err)
		}
		ids[age] = [2]string{run, tomb}
	}

	runRetentionApplyWith(t, project, "retention:\n  memory_eviction_audit_days: 730\n")

	if !rowExists(t, db, "memory_eviction_audit", ids[400][1]) {
		t.Error("a 400-day-old eviction tombstone was deleted although retention.memory_eviction_audit_days is 730")
	}
	if !rowExists(t, db, "memory_eviction_runs", ids[400][0]) {
		t.Error("a 400-day-old eviction run header was deleted although retention.memory_eviction_audit_days is 730")
	}
	if rowExists(t, db, "memory_eviction_audit", ids[800][1]) || rowExists(t, db, "memory_eviction_runs", ids[800][0]) {
		t.Error("the 800-day-old control rows survived: the eviction step did not run")
	}
}

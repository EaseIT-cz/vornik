package api

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // in-memory DB for the orphan-FK probe

	"vornik.io/vornik/internal/persistence"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOrphanFKDB builds a minimal schema mirroring the columns the
// orphan_fk_rows probe touches, then seeds:
//   - one real task (T1)
//   - task_llm_usage: a valid row (→T1), a NULL-task_id background row
//     (kg_extraction), and a true orphan (→deleted task "ghost")
//   - tool_audit_log: a valid row (→T1) and an empty-task_id row
func seedOrphanFKDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// :memory: is per-connection; pin the pool to one conn so seed,
	// probe and delete all hit the same in-memory database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY)`,
		`CREATE TABLE task_llm_usage (id TEXT PRIMARY KEY, task_id TEXT, source TEXT, recorded_at TIMESTAMP)`,
		`CREATE TABLE tool_audit_log (id TEXT PRIMARY KEY, task_id TEXT)`,
		`CREATE TABLE task_watchers (task_id TEXT)`,
		`INSERT INTO tasks (id) VALUES ('T1')`,
		// valid + NULL-background + true-orphan
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u1','T1','workflow_step')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u2',NULL,'kg_extraction')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u3','ghost','workflow_step')`,
		// valid + empty-string task_id (must not count)
		`INSERT INTO tool_audit_log (id, task_id) VALUES ('a1','T1')`,
		`INSERT INTO tool_audit_log (id, task_id) VALUES ('a2','')`,
	}
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
	return db
}

// TestCheckOrphanFKRows_IgnoresTaskLessRows — regression for the
// 2026-06-11 finding that orphan_fk_rows flagged (and --fix DELETED)
// task_llm_usage rows with a NULL task_id. Those are dispatcher /
// background-maintenance cost records that are task-less by design;
// only a row naming a task that no longer exists is a real orphan.
func TestCheckOrphanFKRows_IgnoresTaskLessRows(t *testing.T) {
	h := &DoctorHandlers{db: seedOrphanFKDB(t)}

	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "orphan_fk_rows", got.Name)
	// The one task_llm_usage row naming a deleted task (u3 → "ghost") is a
	// cost-LEDGER row: reported, not warned on (orphan-FK ledger design,
	// 2026-09-24). The NULL-task_id and empty-task_id rows are not counted at
	// all.
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "1 cost-ledger row")

	// --fix removes nothing here: the ledger probe has no delete, and the
	// task-less rows were never orphans.
	fixed := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "OK", fixed.Status)
	assert.Equal(t, 0, fixed.Fixed)

	var llm, audit int
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage`).Scan(&llm))
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log`).Scan(&audit))
	assert.Equal(t, 3, llm, "no task_llm_usage row may be deleted by --fix — it is the cost ledger")
	assert.Equal(t, 2, audit, "empty-task_id audit row must survive --fix")
}

// Orphan-FK ledger design (2026-09-24). On production 5,134 task_llm_usage
// rows (2,123 tasks deleted by a one-time May–June cleanup) held a permanent
// WARNING, and --fix — which runs every repair together — would have deleted
// the record of money spent.
func TestCheckOrphanFKRows_LedgerRowsAreKeptAndPublished(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u4','ghost','workflow_step'), ('u5','ghost2','workflow_step')`)
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "OK", got.Status, "ledger rows alone must not warn")
	assert.Contains(t, got.Message, "3 cost-ledger rows")
	assert.Contains(t, got.Message, "2 deleted tasks")
	assert.Equal(t, 0, got.Fixed)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage WHERE task_id IN ('ghost','ghost2')`).Scan(&n))
	assert.Equal(t, 3, n, "--fix deleted cost-ledger rows")
}

// Garbage stays garbage: a watcher on a missing task still WARNs and is still
// removed; the ledger count is published beside it and does not inflate it.
func TestCheckOrphanFKRows_AWatcherOrphanStillWarnsAndIsRemoved(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO task_watchers (task_id) VALUES ('ghost')`)
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "1 orphan row")
	assert.Contains(t, got.Message, "1 cost-ledger row")

	fixed := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, 1, fixed.Fixed)
	var w int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_watchers`).Scan(&w))
	assert.Equal(t, 0, w)
}

// Review 2026-09-24 impl F1: the recent-escalation path. Cost rows RECORDED in
// the last 30 days for a task that no longer exists mean a task was deleted
// recently — WARNING on the report path AND the --fix path (the fix-path OK
// flip must not reset it), the message leading with the recent count, and no
// row deleted.
func TestCheckOrphanFKRows_RecentLedgerRowsWarnOnBothPaths(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO task_llm_usage (id, task_id, source, recorded_at) VALUES ('r1','gone','workflow_step',?), ('r2','gone','workflow_step',?)`,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-2*time.Hour))
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	for _, fix := range []bool{false, true} {
		got := h.checkOrphanFKRows(t.Context(), fix)
		assert.Equal(t, "WARNING", got.Status, "fix=%v: recent ledger rows must warn", fix)
		assert.True(t, strings.HasPrefix(got.Message, "2 cost-ledger rows for tasks deleted in the last 30 days"),
			"fix=%v: the recent signal must lead the message: %q", fix, got.Message)
		assert.Equal(t, 0, got.Fixed, "fix=%v", fix)
		assert.Equal(t, 3, got.Kept, "fix=%v: u3 (old) + r1 + r2", fix)
	}
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage WHERE task_id='gone'`).Scan(&n))
	assert.Equal(t, 2, n, "recent ledger rows are reported, never deleted")
}

// Impl F2: a clean database makes no kept claim.
func TestCheckOrphanFKRows_ACleanDatabaseMakesNoKeptClaim(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`DELETE FROM task_llm_usage WHERE task_id='ghost'`)
	require.NoError(t, err)
	got := (&DoctorHandlers{db: db}).checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "OK", got.Status)
	assert.Equal(t, "no orphan rows across audit/usage/watchers", got.Message)
	assert.Equal(t, 0, got.Kept)
}

// Impl F3/F4: an audit orphan plus ledger rows — only the audit row warns and
// is removed; the post-fix message names both the cleanup and the kept rows.
func TestCheckOrphanFKRows_AnAuditOrphanBesideTheLedger(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO tool_audit_log (id, task_id) VALUES ('a3','ghost')`)
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "1 orphan rows referencing missing tasks")

	fixed := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "OK", fixed.Status)
	assert.Equal(t, 1, fixed.Fixed)
	assert.Equal(t, "1 orphan rows cleaned up; 1 cost-ledger rows (1 deleted tasks) kept by design", fixed.Message)
	var audit, llm int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log WHERE task_id='ghost'`).Scan(&audit))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage WHERE task_id='ghost'`).Scan(&llm))
	assert.Equal(t, 0, audit, "the audit orphan is removed")
	assert.Equal(t, 1, llm, "the ledger row survives")
}

// Companion tool-audit design (2026-09-24): a companion row's task_id is the
// synthetic session id "companion:<key id>" by design (B-17). It never names a
// task, so it can never dangle — and --fix must not delete the audit trail.
func TestCheckOrphanFKRows_CompanionAuditRowsAreNotOrphans(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO tool_audit_log (id, task_id) VALUES ('c1','companion:akey_1'), ('c2','companion:akey_2')`)
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "OK", got.Status, "companion rows are not orphans")
	assert.Equal(t, 0, got.Fixed)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log WHERE task_id LIKE 'companion:%'`).Scan(&n))
	assert.Equal(t, 2, n, "--fix deleted the companion audit trail")
}

// Companion tool-audit design F7: beside a REAL orphan, the real one is
// counted and removed and the companion row survives.
func TestCheckOrphanFKRows_ARealOrphanBesideACompanionRow(t *testing.T) {
	db := seedOrphanFKDB(t)
	_, err := db.Exec(`INSERT INTO tool_audit_log (id, task_id) VALUES ('c1','companion:akey_1'), ('real','ghost')`)
	require.NoError(t, err)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "1 orphan rows referencing missing tasks")

	fixed := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, 1, fixed.Fixed)
	var orphan, comp int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log WHERE id='real'`).Scan(&orphan))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log WHERE id='c1'`).Scan(&comp))
	assert.Equal(t, 0, orphan, "the real orphan is removed")
	assert.Equal(t, 1, comp, "the companion row survives")
}

// Design F5a: no real task id can begin "companion:" — task ids are
// GenerateID("task"). If that ever changed, the orphan exclusion could mask a
// real dangling reference.
func TestTaskIDsNeverLookLikeCompanionSessionIDs(t *testing.T) {
	for i := 0; i < 50; i++ {
		if id := persistence.GenerateID("task"); strings.HasPrefix(id, "companion:") {
			t.Fatalf("a task id began with the companion session prefix: %q", id)
		}
	}
}

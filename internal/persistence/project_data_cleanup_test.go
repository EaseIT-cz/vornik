package persistence

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProjectDataTables_IncludesDispatcherReminders pins B-12.
// Without this entry, a deleted project's pending reminders fire
// after the project is gone — operator sees a phantom
// Telegram/email reminder from a project they can't navigate to
// anymore. The fix is purely declarative (one row in the
// ProjectDataTables slice) but easy to lose in a future refactor,
// so a name-pinning test guards it.
func TestProjectDataTables_IncludesDispatcherReminders(t *testing.T) {
	want := "dispatcher_reminders"
	for _, table := range ProjectDataTables {
		if table == want {
			return
		}
	}
	t.Fatalf("ProjectDataTables must include %q so project-wide cleanup wipes reminders tied to deleted projects (B-12)", want)
}

// TestProjectDataTables_IncludesWorkflowHealing pins the blackbox
// self-healing cleanup. Both tables carry project_id NOT NULL but
// neither FK-cascades, so without an explicit entry a deleted
// project orphans its open healing triggers (they keep rendering in
// /ui/admin/blackbox/triggers/ for a vanished project) and its
// detection overrides. Declarative, easy to lose in a refactor —
// guard it by name.
func TestProjectDataTables_IncludesWorkflowHealing(t *testing.T) {
	for _, want := range []string{"workflow_healing_triggers", "workflow_healing_overrides"} {
		found := false
		for _, table := range ProjectDataTables {
			if table == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProjectDataTables must include %q so project-wide cleanup wipes orphaned blackbox healing rows", want)
		}
	}
}

// TestProjectDataTables_OrderingPutsParentsLast pins the tasks/
// executions ordering invariant the cleanup transaction relies on.
// Child tables (task_messages, task_scratchpad, …) cascade off
// tasks via ON DELETE CASCADE; deleting tasks too early would
// strand the explicit children-with-project_id passes that come
// after. tasks + executions therefore must appear AFTER the
// project-scoped audit tables.
func TestProjectDataTables_OrderingPutsParentsLast(t *testing.T) {
	idx := func(name string) int {
		for i, t := range ProjectDataTables {
			if t == name {
				return i
			}
		}
		return -1
	}
	tasksIdx := idx("tasks")
	execsIdx := idx("executions")
	auditsIdx := idx("tool_audit_log")
	assert.Greater(t, tasksIdx, auditsIdx,
		"tasks must come AFTER tool_audit_log so audit deletes complete before their parent cascades")
	assert.Greater(t, tasksIdx, execsIdx,
		"tasks must come AFTER executions so executions delete cleanly before tasks cascades fire")
}

// GitHub #370 (operator decision 2026-10-14): mcp_oauth_tokens and the five
// memory audit tables moved out of ProjectScopedTablesNotWiped into the wipe.
// The UNREVIEWED count is pinned so a move is deliberate in both directions:
// it drops by exactly the tables reviewed (26 before #370, 20 after).
func TestProjectScopedTablesNotWiped_UnreviewedCount(t *testing.T) {
	n := 0
	for _, reason := range ProjectScopedTablesNotWiped {
		if reason == notWipedUnreviewed {
			n++
		}
	}
	if n != 16 {
		t.Errorf("UNREVIEWED entries = %d, want 16 (26 before GitHub #370, minus the six it moved into the wipe, minus the four grant/approval tables of archival design section I)", n)
	}
}

// GitHub #370: the six tables are wiped on every driver that has them, and
// memory_quarantine (SQLite's counterpart of project_memory_quarantine) only
// on SQLite: ProjectDataTablesFor(true) must not name it, or the Postgres wipe
// fails at "relation does not exist" and rolls back, deleting nothing.
func TestProjectDataTablesFor_Issue370Tables(t *testing.T) {
	has := func(list []string, want string) bool {
		for _, tb := range list {
			if tb == want {
				return true
			}
		}
		return false
	}
	pg, lite := ProjectDataTablesFor(true), ProjectDataTablesFor(false)
	for _, tb := range []string{"mcp_oauth_tokens", "memory_ingest_audit", "memory_retrieval_audit",
		"memory_search_stages", "memory_policy_evaluations"} {
		if !has(pg, tb) || !has(lite, tb) {
			t.Errorf("%s: wiped on postgres=%v sqlite=%v, want both", tb, has(pg, tb), has(lite, tb))
		}
		if _, excluded := ProjectScopedTablesNotWiped[tb]; excluded {
			t.Errorf("%s is still in ProjectScopedTablesNotWiped", tb)
		}
	}
	if !has(lite, "memory_quarantine") || has(pg, "memory_quarantine") {
		t.Errorf("memory_quarantine: wiped on postgres=%v sqlite=%v, want sqlite only",
			has(pg, "memory_quarantine"), has(lite, "memory_quarantine"))
	}
	for tb := range ProjectDataTablesSQLiteOnly {
		if ProjectDataTablesPostgresOnly[tb] {
			t.Errorf("%s is flagged both SQLiteOnly and PostgresOnly: no driver would wipe it", tb)
		}
	}
}

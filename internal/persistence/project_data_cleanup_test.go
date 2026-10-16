package persistence

import (
	"sort"
	"strings"
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

// GitHub #370, archival design section J (2026-10-16; found by the W8 census,
// "UNREVIEWED 16"): every project table the wipe does not delete has a
// decided reason. A reason is either "removed with its parent" (a cascade
// child) or "kept: <why>"; there is no third, undecided category any more.
// The kept set is pinned by name, so keeping a new table's rows past its
// project is a reviewed diff to this test, not a default.
func TestProjectScopedTablesNotWiped_EveryReasonDecided(t *testing.T) {
	var kept []string
	for tb, reason := range ProjectScopedTablesNotWiped {
		switch {
		case strings.HasPrefix(reason, NotWipedCascadePrefix):
		case strings.HasPrefix(reason, NotWipedKeptPrefix):
			if strings.TrimSpace(strings.TrimPrefix(reason, NotWipedKeptPrefix)) == "" {
				t.Errorf("%s is kept with no reason", tb)
			}
			kept = append(kept, tb)
		default:
			t.Errorf("%s: reason %q is neither a cascade (%q) nor a kept (%q) decision", tb, reason, NotWipedCascadePrefix, NotWipedKeptPrefix)
		}
	}
	sort.Strings(kept)
	if got, want := strings.Join(kept, ","), "secret_redaction_audit"; got != want {
		t.Errorf("kept tables = %s, want %s (archival design section J)", got, want)
	}
}

// Section J: the partly-wiped tables are exactly these three. Each is listed
// (so the wipe reaches it), not also excluded, not a SQLite cascade parent
// (the cascade executor would apply its children to rows the predicate
// keeps), and states both its predicate and what it keeps.
// data_subject_links runs last: its predicate asks whether the linked row
// still exists, which is only true after every other table is wiped.
func TestProjectDataTablesPartial_Pinned(t *testing.T) {
	var names []string
	for tb, pw := range ProjectDataTablesPartial {
		names = append(names, tb)
		if strings.TrimSpace(pw.Where) == "" || strings.TrimSpace(pw.Keeps) == "" {
			t.Errorf("%s: Where %q / Keeps %q must both be set", tb, pw.Where, pw.Keeps)
		}
		if strings.Contains(pw.Where, "?") || strings.Contains(pw.Where, "$") {
			t.Errorf("%s: Where must not carry a placeholder: %q", tb, pw.Where)
		}
		if _, ex := ProjectScopedTablesNotWiped[tb]; ex {
			t.Errorf("%s is partly wiped and also excluded", tb)
		}
		if SQLiteCascadeParent(tb) {
			t.Errorf("%s is partly wiped and a SQLite cascade parent", tb)
		}
		found := false
		for _, l := range ProjectDataTables {
			found = found || l == tb
		}
		if !found {
			t.Errorf("%s is partly wiped but not in ProjectDataTables", tb)
		}
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "control_plane_proposals,data_subject_links,project_skills"; got != want {
		t.Errorf("partly-wiped tables = %s, want %s (archival design section J)", got, want)
	}
	if last := ProjectDataTables[len(ProjectDataTables)-1]; last != "data_subject_links" {
		t.Errorf("last table of the wipe = %s, want data_subject_links", last)
	}
}

// Section J: the data_subject_links predicate removes a link only when its
// row is gone, for each linkable table the wipe removes.
func TestProjectDataTablesPartial_LinkPredicateNamesEveryWipedLinkTable(t *testing.T) {
	where := ProjectDataTablesPartial["data_subject_links"].Where
	for _, tb := range DataSubjectLinkTablesWiped {
		want := "table_name = '" + tb + "' AND NOT EXISTS (SELECT 1 FROM " + tb + " x WHERE x.id = data_subject_links.row_id)"
		if !strings.Contains(where, want) {
			t.Errorf("predicate lacks %q:\n%s", want, where)
		}
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

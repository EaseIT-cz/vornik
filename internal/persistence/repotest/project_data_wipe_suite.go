package repotest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunProjectDataWipeSuite pins the archived-project wipe
// (persistence.ProjectDataDeleter) on a REAL migrated database.
//
// It seeds two projects — a victim and a control — with one row in every
// table of persistence.ProjectDataTables the driver has, and one row in every
// child table a CASCADE / SET NULL edge reaches from those tables (the
// driver's declared foreign keys plus persistence.SQLiteCascadeGraph), then
// wipes the victim and asserts:
//   - no listed table holds a row of the victim;
//   - no child row still references one of the victim's deleted parents;
//   - every row of the control project is still there.
//
// Rows are generated from the driver's own catalog (columns, NOT NULL,
// defaults, foreign keys), so a new table needs no hand-written seeder; a
// column whose CHECK constraint a generated value cannot meet gets an entry
// in wipeValueOverrides.
//
// GitHub #365 (2026-10-13): on SQLite the wipe failed at the first
// Postgres-only table (`no such table: memory_embed_queue`) and rolled back,
// deleting nothing — a silent failure on a data-deletion path.
//
// see LLD § https://docs.vornik.io "2026-10-13 — the wipe on SQLite"
func RunProjectDataWipeSuite(t *testing.T, db *sql.DB, deleter persistence.ProjectDataDeleter, postgres bool) {
	t.Helper()
	f := &wipeFixture{t: t, ctx: context.Background(), db: db, postgres: postgres,
		cat: loadWipeCatalog(t, db, postgres), seeded: map[string]map[string]map[string]any{},
		rowsIn: map[string]map[string]int{}, keptRows: map[string]map[string]int{}, support: map[string][]any{}}
	f.plan()
	inList := len(f.listed) - f.unlisted + len(f.partial)
	t.Logf("wipe suite denominator (%s): %d listed tables present (%d of them partly wiped), %d of %d listed absent on this driver; %d required tables outside the list (wipeMustRemove); %d kept tables seeded; %d cascade edges into %d child tables seeded",
		driverName(postgres), inList, len(f.partial), len(persistence.ProjectDataTables)-inList, len(persistence.ProjectDataTables),
		f.unlisted, len(f.kept), len(f.edges), len(f.childSet))

	victim, control := uniqueID("wipe-victim"), uniqueID("wipe-control")
	// Both projects go when the suite ends, even when verification fails: the
	// Postgres lane shares one database across runs, and the control project's
	// rows used to stay in every project table (incident 2026-10-14: ten
	// leftover 'firing' reminders filled TestReclaimStuckFiring's page). The
	// deleter removes what it covers; the rows the wipe keeps by design (kept
	// tables, partly-wiped tables' keep rows) and the support parents are
	// removed explicitly.
	t.Cleanup(func() {
		for _, p := range []string{victim, control} {
			if _, err := deleter.DeleteProjectData(context.Background(), p); err != nil {
				t.Errorf("cleanup: DeleteProjectData(%s): %v", p, err)
			}
		}
		f.cleanupKept(victim, control)
	})
	f.seedProject(victim)
	f.seedProject(control)

	if _, err := deleter.DeleteProjectData(f.ctx, victim); err != nil {
		t.Fatalf("DeleteProjectData(%s): %v", victim, err)
	}
	f.verifyListed(victim, control)
	f.verifyChildren(victim, control)
	f.verifyKept(victim, control)
	f.verifyExactlyKept(victim)
	f.verifyScrubbed(victim, control)
	f.verifyUnscopedLinks(victim, control)
}

// verifyScrubbed: the victim's kept proposal lost its free text (section J,
// review round 1 HIGH); the control's kept proposal is untouched.
func (f *wipeFixture) verifyScrubbed(victim, control string) {
	if f.cat.tables["control_plane_proposals"] == nil {
		return
	}
	var title, rationale, evidence string
	q := `SELECT title, rationale, evidence FROM control_plane_proposals WHERE project_id = $1 AND blast_radius = 'daemon'`
	if err := f.db.QueryRowContext(f.ctx, q, victim).Scan(&title, &rationale, &evidence); err != nil {
		f.t.Errorf("victim's kept proposal: %v", err)
	} else if title != "proposal of deleted project "+victim || rationale != "" || evidence != "" {
		f.t.Errorf("victim's kept proposal not scrubbed: title %q rationale %q evidence %q", title, rationale, evidence)
	}
	if err := f.db.QueryRowContext(f.ctx, q, control).Scan(&title, &rationale, &evidence); err != nil {
		f.t.Errorf("control's kept proposal: %v", err)
	} else if title != "kept title "+control || rationale == "" || evidence == "" {
		f.t.Errorf("control's kept proposal was changed: title %q rationale %q evidence %q", title, rationale, evidence)
	}
}

// verifyUnscopedLinks: a link with no project_id to the victim's wiped
// artifact is removed with the row; one to the control's surviving artifact
// stays (section J, review round 1 MEDIUM).
func (f *wipeFixture) verifyUnscopedLinks(victim, control string) {
	if f.cat.tables["data_subject_links"] == nil {
		return
	}
	q := `SELECT count(*) FROM data_subject_links WHERE project_id IS NULL AND table_name = 'artifacts' AND row_id = $1`
	if n := f.count(q, f.seeded[victim]["artifacts"]["id"]); n != 0 {
		f.t.Errorf("data_subject_links: %d unscoped link(s) to the wiped project's artifact remain", n)
	}
	if n := f.count(q, f.seeded[control]["artifacts"]["id"]); n != 1 {
		f.t.Errorf("data_subject_links: %d unscoped link(s) to the control project's artifact, want 1", n)
	}
}

// wipeMustKeep is the designed residue of a wipe (archival design section J,
// GitHub #370, 2026-10-16): the project's rows that survive, by table. A kept
// table keeps every row; a partly-wiped table keeps the rows its predicate
// does not match. After the wipe, the victim's rows must remain in exactly
// these tables (those present on the driver) and nowhere else. Pinned here,
// independently of persistence.ProjectScopedTablesNotWiped and
// persistence.ProjectDataTablesPartial, so a change to either list must
// change this one too.
var wipeMustKeep = map[string]string{
	"secret_redaction_audit":  "kept table",
	"project_skills":          "partly wiped: global skills stay",
	"control_plane_proposals": "partly wiped: radius beyond the project stays",
	"data_subject_links":      "partly wiped: links to surviving rows stay",
}

// wipePartialRows gives, for each partly-wiped table, the fixed column values
// of one row the predicate must remove and one it must keep. Every table of
// persistence.ProjectDataTablesPartial needs an entry: plan fails otherwise,
// so a new predicate cannot ship untested.
var wipePartialRows = map[string]func(f *wipeFixture, p string) (remove, keep map[string]any){
	"project_skills": func(*wipeFixture, string) (map[string]any, map[string]any) {
		return map[string]any{"is_global": false}, map[string]any{"is_global": true}
	},
	// The kept (daemon-radius) proposal carries free text, which the wipe must
	// scrub for the victim and leave alone for the control (verifyScrubbed).
	"control_plane_proposals": func(_ *wipeFixture, p string) (map[string]any, map[string]any) {
		return map[string]any{"blast_radius": "project"},
			map[string]any{"blast_radius": "daemon", "title": "kept title " + p,
				"rationale": "quotes a task of " + p, "evidence": `{"root_cause":"from ` + p + `"}`}
	},
	// The removed link points at the project's own seeded artifacts row,
	// which the wipe deletes; the kept one at a channel_sessions row id
	// (a table the wipe never touches), whose person must stay findable.
	"data_subject_links": func(f *wipeFixture, p string) (map[string]any, map[string]any) {
		return map[string]any{"table_name": "artifacts", "row_id": f.seeded[p]["artifacts"]["id"]},
			map[string]any{"table_name": "channel_sessions", "row_id": "telegram/" + p}
	},
}

// wipeMustRemove names project tables the wipe is required to remove on
// every driver that has them, independently of persistence.ProjectDataTables:
// they are seeded and verified like a listed table, so dropping one from the
// list fails this suite instead of leaving its rows behind silently.
//
// GitHub #370 (operator decision 2026-10-14, release 2026.10.6): deleting a
// project left its OAuth tokens (mcp_oauth_tokens) and its memory audit rows,
// which hold content or query text, behind on both drivers.
// memory_quarantine exists on SQLite only.
var wipeMustRemove = []string{
	"mcp_oauth_tokens",
	"memory_ingest_audit",
	"memory_retrieval_audit",
	"memory_search_stages",
	"memory_policy_evaluations",
	"memory_quarantine",
	// Archival design section I (#370 carry-over, delta audit 2026-10-14):
	// authorisations of a deleted project, which a project re-created under
	// the same ID inherited, and the deleted executions' tool-grant trail.
	"broker_standing_grants",
	"agent_workflow_approvals",
	"agent_integration_approvals",
	"execution_tool_grants",
	// Archival design section J (GitHub #370, W8 census "UNREVIEWED 16",
	// 2026-10-16): the twelve wholly wiped. Before J their rows outlived the
	// project on both drivers; five of them a re-created project inherited
	// (budget_reservations, forge_*, group_projects, instincts, and
	// project_first_seen's telemetry marker).
	"blackbox_trace_cache",
	"broker_actions",
	"budget_reservations",
	"fixit_sessions",
	"forge_ci_outcomes",
	"forge_pr_review_state",
	"group_projects",
	"instincts",
	"project_first_seen",
	"recovery_events",
	"trading_fills_shadow",
	"web_write_actions",
}

type wipeEdge struct{ child, col, parent, ref string }

type wipeFixture struct {
	t        *testing.T
	ctx      context.Context
	db       *sql.DB
	postgres bool
	cat      *wipeCatalog

	listed    []string // ProjectDataTables present on this driver, list order, then wipeMustRemove extras
	listedSet map[string]bool
	unlisted  int        // wipeMustRemove tables present here that ProjectDataTables does not name
	edges     []wipeEdge // cascade edges from a listed parent into an unlisted child
	childSet  map[string]bool

	seeded map[string]map[string]map[string]any // project -> table -> column -> value
	rowsIn map[string]map[string]int            // project -> listed table -> rows seeded

	kept    []string // kept tables present on this driver (whole rows survive)
	partial []string // partly-wiped tables present on this driver
	// keptRows: project -> table -> rows that must survive the wipe (kept
	// tables and partly-wiped tables' keep rows).
	keptRows map[string]map[string]int
	// support: rows of parent tables outside the wipe (data_subjects, groups)
	// seeded so a declared foreign key out of a wiped table is satisfied;
	// removed explicitly at cleanup. table -> ids.
	support map[string][]any
}

// plan picks the listed tables the driver has and the child tables an edge
// from them reaches that are not themselves listed (Postgres removes those by
// cascade): the driver's declared c/n foreign keys plus SQLiteCascadeGraph.
func (f *wipeFixture) plan() {
	f.listedSet, f.childSet = map[string]bool{}, map[string]bool{}
	for _, tb := range persistence.ProjectDataTables {
		if f.cat.tables[tb] == nil {
			continue
		}
		// A partly-wiped table is seeded with one row its predicate removes
		// and one it keeps (seedPartial), not as a wholly wiped table.
		if _, partial := persistence.ProjectDataTablesPartial[tb]; partial {
			if wipePartialRows[tb] == nil {
				f.t.Fatalf("%s is partly wiped (persistence.ProjectDataTablesPartial) but wipePartialRows has no remove/keep rows for it", tb)
			}
			f.partial = append(f.partial, tb)
			continue
		}
		f.listed = append(f.listed, tb)
		f.listedSet[tb] = true
	}
	for _, tb := range sortedKeys(persistence.ProjectScopedTablesNotWiped) {
		if strings.HasPrefix(persistence.ProjectScopedTablesNotWiped[tb], persistence.NotWipedKeptPrefix) && f.cat.tables[tb] != nil {
			f.kept = append(f.kept, tb)
		}
	}
	for _, tb := range wipeMustRemove {
		if f.cat.tables[tb] != nil && !f.listedSet[tb] {
			f.listed = append(f.listed, tb)
			f.listedSet[tb] = true
			f.unlisted++
		}
	}
	for _, tb := range sortedKeys(f.cat.tables) {
		for _, fk := range f.cat.tables[tb].fks {
			if fk.cascadeOrNull {
				f.addEdge(wipeEdge{tb, fk.col, fk.parent, fk.ref})
			}
		}
	}
	for _, e := range persistence.SQLiteCascadeGraph {
		if !e.PostgresOnly || f.postgres {
			f.addEdge(wipeEdge{e.Child, e.Column, e.Parent, "id"})
		}
	}
}

func (f *wipeFixture) addEdge(e wipeEdge) {
	child := f.cat.tables[e.child]
	if f.listedSet[e.child] || !f.listedSet[e.parent] || child == nil || child.col(e.col) == nil {
		return
	}
	for _, o := range f.edges {
		if o.child == e.child && o.col == e.col && o.parent == e.parent {
			return
		}
	}
	f.edges = append(f.edges, e)
	f.childSet[e.child] = true
}

// seedProject seeds one row in every listed table (parents first: the list is
// children-first, so in reverse), then the child tables.
func (f *wipeFixture) seedProject(p string) {
	f.seeded[p] = map[string]map[string]any{}
	f.rowsIn[p] = map[string]int{}
	for i := len(f.listed) - 1; i >= 0; i-- {
		f.seeded[p][f.listed[i]] = f.seed(p, f.listed[i], nil)
		f.rowsIn[p][f.listed[i]]++
	}
	for _, child := range sortedKeys(f.childSet) {
		f.seedChild(p, child)
	}
	f.keptRows[p] = map[string]int{}
	for _, tb := range f.kept {
		f.seed(p, tb, nil)
		f.keptRows[p][tb]++
	}
	for _, tb := range f.partial {
		remove, keep := wipePartialRows[tb](f, p)
		f.seed(p, tb, remove)
		f.seed(p, tb, keep)
		f.rowsIn[p][tb] += 2
		f.keptRows[p][tb]++
	}
	// An unscoped link (project_id NULL, as an operator assertion without
	// --project writes it) to this project's artifact, under its own subject
	// so it does not collide with the scoped link's primary key.
	if f.cat.tables["data_subject_links"] != nil && f.cat.tables["data_subjects"] != nil {
		subj := f.seed(p, "data_subjects", nil)
		f.support["data_subjects"] = append(f.support["data_subjects"], subj["id"])
		f.seed(p, "data_subject_links", map[string]any{"project_id": nil, "subject_id": subj["id"],
			"table_name": "artifacts", "row_id": f.seeded[p]["artifacts"]["id"]})
	}
}

// seedSupportParents seeds one row of every parent a declared foreign key of
// table points at that this project has not seeded and the wipe does not
// list (data_subject_links -> data_subjects, group_projects -> groups), so
// the generated row satisfies the key. They are not project data.
func (f *wipeFixture) seedSupportParents(p, table string) {
	tb := f.cat.tables[table]
	for _, fk := range tb.fks {
		c := tb.col(fk.col)
		if c == nil || !c.notNull || c.hasDefault || fk.parent == table || f.seeded[p][fk.parent] != nil ||
			f.listedSet[fk.parent] || f.cat.tables[fk.parent] == nil {
			continue
		}
		row := f.seed(p, fk.parent, nil)
		f.seeded[p][fk.parent] = row
		f.support[fk.parent] = append(f.support[fk.parent], row[fk.ref])
	}
}

func (f *wipeFixture) seed(p, table string, fixed map[string]any) map[string]any {
	f.t.Helper()
	f.seedSupportParents(p, table)
	row, err := seedWipeRow(f.ctx, f.db, f.cat.tables[table], p, f.seeded[p], f.postgres, fixed)
	if err != nil {
		f.t.Fatalf("seed %s for %s: %v", table, p, err)
	}
	return row
}

// seedChild seeds one row of child with every edge column at this project's
// parent — except a table with two edges to ONE parent
// (knowledge_edges.from_entity / .to_entity, CHECK from <> to), which gets a
// row per edge: that edge's column at the parent, the sibling column at a
// spare row of the same parent and project.
func (f *wipeFixture) seedChild(p, child string) {
	var mine []wipeEdge
	perParent := map[string]int{}
	for _, e := range f.edges {
		if e.child == child {
			mine = append(mine, e)
			perParent[e.parent]++
		}
	}
	spares := map[string]map[string]any{}
	for parent, n := range perParent {
		if n > 1 {
			spares[parent] = f.seed(p, parent, nil)
			f.rowsIn[p][parent]++
		}
	}
	if len(spares) == 0 {
		f.seed(p, child, nil)
		return
	}
	for _, e := range mine {
		fixed := map[string]any{}
		for _, o := range mine {
			if o.parent == e.parent && o.col != e.col {
				fixed[o.col] = spares[o.parent][o.ref]
			}
		}
		f.seed(p, child, fixed)
	}
}

func (f *wipeFixture) count(q string, arg any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(f.ctx, q, arg).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *wipeFixture) verifyListed(victim, control string) {
	for _, tb := range f.listed {
		q := `SELECT count(*) FROM ` + tb + ` WHERE project_id = $1`
		if n := f.count(q, victim); n != 0 {
			f.t.Errorf("%s: %d row(s) of the wiped project remain", tb, n)
		}
		if n := f.count(q, control); n != f.rowsIn[control][tb] {
			f.t.Errorf("%s: control project has %d row(s), want %d (the wipe reached another project)", tb, n, f.rowsIn[control][tb])
		}
	}
}

func (f *wipeFixture) verifyChildren(victim, control string) {
	for _, e := range f.edges {
		vRef, cRef := f.seeded[victim][e.parent][e.ref], f.seeded[control][e.parent][e.ref]
		if vRef == nil || cRef == nil {
			f.t.Errorf("%s.%s -> %s.%s: parent row has no %s value; seeder gap", e.child, e.col, e.parent, e.ref, e.ref)
			continue
		}
		q := `SELECT count(*) FROM ` + e.child + ` WHERE ` + e.col + ` = $1`
		if n := f.count(q, vRef); n != 0 {
			f.t.Errorf("%s.%s: %d row(s) still reference the wiped project's %s %v", e.child, e.col, n, e.parent, vRef)
		}
		if n := f.count(q, cRef); n < 1 {
			f.t.Errorf("%s.%s: the control project's row referencing %s %v is gone", e.child, e.col, e.parent, cRef)
		}
	}
}

// verifyKept: every row the design keeps is still there, the victim's and
// the control project's; and of a partly-wiped table the victim keeps only
// its keep row while the control keeps both.
func (f *wipeFixture) verifyKept(victim, control string) {
	for _, tb := range append(append([]string{}, f.kept...), f.partial...) {
		q := `SELECT count(*) FROM ` + tb + ` WHERE project_id = $1`
		if n := f.count(q, victim); n != f.keptRows[victim][tb] {
			f.t.Errorf("%s: wiped project has %d row(s) left, want %d (%s)", tb, n, f.keptRows[victim][tb], wipeMustKeep[tb])
		}
		want := f.keptRows[control][tb]
		if _, partial := persistence.ProjectDataTablesPartial[tb]; partial {
			want = f.rowsIn[control][tb]
		}
		if n := f.count(q, control); n != want {
			f.t.Errorf("%s: control project has %d row(s), want %d (the wipe reached another project)", tb, n, want)
		}
	}
}

// verifyExactlyKept: after the wipe the victim's rows remain in exactly the
// designed tables (wipeMustKeep, those this driver has) and in no other table
// with a project_id column. It reads every such table, so it publishes its
// denominator.
func (f *wipeFixture) verifyExactlyKept(victim string) {
	scoped := 0
	var left []string
	for _, tb := range sortedKeys(f.cat.tables) {
		if f.cat.tables[tb].col("project_id") == nil {
			continue
		}
		scoped++
		if f.count(`SELECT count(*) FROM `+tb+` WHERE project_id = $1`, victim) > 0 {
			left = append(left, tb)
		}
	}
	var want []string
	for _, tb := range sortedKeys(wipeMustKeep) {
		if f.cat.tables[tb] != nil {
			want = append(want, tb)
		}
	}
	if strings.Join(left, ",") != strings.Join(want, ",") {
		f.t.Errorf("after the wipe the deleted project's rows remain in %v, want exactly %v (archival design section J)", left, want)
	}
	f.t.Logf("residue check (%s): %d tables with a project_id column examined; victim rows remain in %d: %v",
		driverName(f.postgres), scoped, len(left), left)
}

// cleanupKept removes what the deleter leaves by design: the kept and
// partly-wiped tables' remaining rows of both projects, and the support
// parents.
func (f *wipeFixture) cleanupKept(projects ...string) {
	ctx := context.Background()
	for _, tb := range append(append([]string{}, f.kept...), f.partial...) {
		for _, p := range projects {
			if _, err := f.db.ExecContext(ctx, `DELETE FROM `+tb+` WHERE project_id = $1`, p); err != nil {
				f.t.Errorf("cleanup %s: %v", tb, err)
			}
		}
	}
	if f.cat.tables["data_subject_links"] != nil {
		for _, id := range f.support["data_subjects"] {
			if _, err := f.db.ExecContext(ctx, `DELETE FROM data_subject_links WHERE subject_id = $1`, id); err != nil {
				f.t.Errorf("cleanup unscoped links: %v", err)
			}
		}
	}
	for _, tb := range sortedKeys(f.support) {
		for _, id := range f.support[tb] {
			if _, err := f.db.ExecContext(ctx, `DELETE FROM `+tb+` WHERE id = $1`, id); err != nil {
				f.t.Errorf("cleanup support row %s %v: %v", tb, id, err)
			}
		}
	}
}

func driverName(postgres bool) string {
	if postgres {
		return "postgres"
	}
	return "sqlite"
}

// wipeValueOverrides are values for columns a generated value cannot satisfy
// (CHECK constraints, enums). Keyed table.column; valid on both drivers.
var wipeValueOverrides = map[string]any{
	"task_messages.author_kind":        "system",
	"task_messages.message_kind":       "note",
	"webhook_events.status":            "processed",
	"autonomy_evaluations.outcome":     "enqueued",
	"trading_orders.mode":              "paper",
	"trading_orders.action":            "BUY",
	"trading_orders.order_type":        "MKT",
	"trading_orders.status":            "SUBMITTED",
	"task_judge_verdicts.verdict":      "pass",
	"task_credentials.tool":            "git",
	"execution_quality_scores.status":  "unscorable",
	"execution_narration.kind":         "step",
	"artifacts.artifact_class":         "OUTPUT",
	"memory_ingest_audit.decision":     "admitted",
	"agent_integration_approvals.kind": "mcp",
	"broker_standing_grants.max_uses":  5,
	"broker_standing_grants.uses_left": 5,
	"instincts.domain":                 "quality",
	"broker_actions.status":            "pending",
	"control_plane_proposals.kind":     "config",
	"groups.role":                      "user",
}

type wipeFK struct {
	col, parent, ref string
	cascadeOrNull    bool
}

type wipeCol struct {
	name, typ  string
	notNull    bool
	hasDefault bool
	generated  bool
}

type wipeTable struct {
	name string
	cols []wipeCol
	fks  []wipeFK
}

func (w *wipeTable) col(name string) *wipeCol {
	for i := range w.cols {
		if w.cols[i].name == name {
			return &w.cols[i]
		}
	}
	return nil
}

type wipeCatalog struct{ tables map[string]*wipeTable }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProjectScopedTables returns every base table with a project_id column on
// the driver db is, sorted — the denominator the wipe guard compares
// persistence.ProjectDataTables and persistence.ProjectScopedTablesNotWiped
// against.
func ProjectScopedTables(t *testing.T, db *sql.DB, postgres bool) []string {
	t.Helper()
	cat := loadWipeCatalog(t, db, postgres)
	var out []string
	for _, tb := range sortedKeys(cat.tables) {
		if cat.tables[tb].col("project_id") != nil {
			out = append(out, tb)
		}
	}
	return out
}

// SecretRedactionAuditKeptColumns is secret_redaction_audit's column set,
// sorted, as the project wipe's kept decision read it (archival design J.2):
// ids, a sink name, a pattern name, a count. Both driver guards compare
// against it.
const SecretRedactionAuditKeptColumns = "checkpoint,count,created_at,execution_id,finding_type,id,project_id,source,task_id"

// TableColumns returns table's column names on the driver db is, sorted.
func TableColumns(t *testing.T, db *sql.DB, postgres bool, table string) []string {
	t.Helper()
	tb := loadWipeCatalog(t, db, postgres).tables[table]
	if tb == nil {
		t.Fatalf("no table %s", table)
	}
	var out []string
	for _, c := range tb.cols {
		out = append(out, c.name)
	}
	sort.Strings(out)
	return out
}

// CheckScrubComplement fails unless control_plane_proposals.blast_radius is
// NOT NULL on the driver db is. The wipe deletes the project's proposals
// matching blast_radius = 'project' and scrubs those matching NOT (...); with
// a NULL radius neither predicate would match and the row would survive
// unscrubbed (archival design J.3; code review round 1, W10-B).
func CheckScrubComplement(t *testing.T, db *sql.DB, postgres bool) {
	t.Helper()
	tb := loadWipeCatalog(t, db, postgres).tables["control_plane_proposals"]
	if tb == nil || tb.col("blast_radius") == nil {
		t.Fatal("control_plane_proposals.blast_radius not found")
	}
	if !tb.col("blast_radius").notNull {
		t.Error("control_plane_proposals.blast_radius is nullable: the wipe's delete (blast_radius = 'project') and scrub (NOT (...)) no longer cover every row; extend ProjectDataTablesPartial before relaxing the column")
	}
}

// TableExists reports whether a base table exists on the driver db is.
func TableExists(t *testing.T, db *sql.DB, postgres bool, table string) bool {
	t.Helper()
	return loadWipeCatalog(t, db, postgres).tables[table] != nil
}

func loadWipeCatalog(t *testing.T, db *sql.DB, postgres bool) *wipeCatalog {
	t.Helper()
	cat := &wipeCatalog{tables: map[string]*wipeTable{}}
	if postgres {
		loadPostgresWipeCatalog(t, db, cat)
	} else {
		loadSQLiteWipeCatalog(t, db, cat)
	}
	return cat
}

// catalogQuery runs q and hands every row to scan, failing the test on error.
func catalogQuery(t *testing.T, db *sql.DB, q string, scan func(*sql.Rows) error, args ...any) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("catalog %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			t.Fatalf("catalog scan: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog rows: %v", err)
	}
}

func loadPostgresWipeCatalog(t *testing.T, db *sql.DB, cat *wipeCatalog) {
	t.Helper()
	catalogQuery(t, db, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		cat.tables[n] = &wipeTable{name: n}
		return nil
	})
	catalogQuery(t, db, `SELECT table_name, column_name, udt_name, is_nullable = 'NO',
			column_default IS NOT NULL OR is_identity = 'YES', is_generated <> 'NEVER'
		FROM information_schema.columns WHERE table_schema = current_schema()
		ORDER BY table_name, ordinal_position`, func(r *sql.Rows) error {
		var tb string
		var c wipeCol
		if err := r.Scan(&tb, &c.name, &c.typ, &c.notNull, &c.hasDefault, &c.generated); err != nil {
			return err
		}
		if w := cat.tables[tb]; w != nil {
			w.cols = append(w.cols, c)
		}
		return nil
	})
	catalogQuery(t, db, `SELECT cl.relname, a.attname, pcl.relname, pa.attname, c.confdeltype::text
		FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = current_schema()
		JOIN pg_class pcl ON pcl.oid = c.confrelid
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		JOIN pg_attribute pa ON pa.attrelid = c.confrelid AND pa.attnum = c.confkey[1]
		WHERE c.contype = 'f' AND array_length(c.conkey, 1) = 1`, func(r *sql.Rows) error {
		var tb, action string
		var fk wipeFK
		if err := r.Scan(&tb, &fk.col, &fk.parent, &fk.ref, &action); err != nil {
			return err
		}
		fk.cascadeOrNull = action == "c" || action == "n"
		if w := cat.tables[tb]; w != nil {
			w.fks = append(w.fks, fk)
		}
		return nil
	})
}

func loadSQLiteWipeCatalog(t *testing.T, db *sql.DB, cat *wipeCatalog) {
	t.Helper()
	var names []string
	catalogQuery(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
		return nil
	})
	for _, n := range names {
		w := &wipeTable{name: n}
		cat.tables[n] = w
		catalogQuery(t, db, `SELECT name, type, "notnull", dflt_value IS NOT NULL, hidden FROM pragma_table_xinfo(?)`, func(r *sql.Rows) error {
			var c wipeCol
			var hidden int
			if err := r.Scan(&c.name, &c.typ, &c.notNull, &c.hasDefault, &hidden); err != nil {
				return err
			}
			c.typ = strings.ToLower(c.typ)
			c.generated = hidden != 0
			w.cols = append(w.cols, c)
			return nil
		}, n)
		catalogQuery(t, db, `SELECT "from", "table", coalesce("to", 'id'), on_delete FROM pragma_foreign_key_list(?)`, func(r *sql.Rows) error {
			var fk wipeFK
			var action string
			if err := r.Scan(&fk.col, &fk.parent, &fk.ref, &action); err != nil {
				return err
			}
			fk.cascadeOrNull = action == "CASCADE" || action == "SET NULL"
			w.fks = append(w.fks, fk)
			return nil
		}, n)
	}
}

var wipeSeq = func() *atomic.Int64 {
	var a atomic.Int64
	a.Store(time.Now().UnixNano() / 1000 % 1_000_000_000)
	return &a
}()

// seedWipeRow inserts one row of tb for project and returns the values it
// wrote. Columns filled: project_id, every foreign-key column whose parent was
// seeded for this project (declared keys and SQLiteCascadeGraph edges), and
// every NOT NULL column without a default; fixed wins over all of them.
func seedWipeRow(ctx context.Context, db *sql.DB, tb *wipeTable, project string,
	parents map[string]map[string]any, postgres bool, fixed map[string]any) (map[string]any, error) {
	refs := wipeRefs(tb, postgres)
	row := map[string]any{}
	var cols, ph []string
	var args []any
	for _, c := range tb.cols {
		v, ok := wipeColumnValue(tb, c, project, refs, parents, postgres, fixed)
		if !ok {
			continue
		}
		cols = append(cols, c.name)
		args = append(args, v)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
		row[c.name] = v
	}
	q := `INSERT INTO ` + tb.name + ` (` + strings.Join(cols, ", ") + `) VALUES (` + strings.Join(ph, ", ") + `)`
	_, hasID := row["id"]
	if hasID || tb.col("id") == nil {
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return nil, fmt.Errorf("%s: %w", q, err)
		}
		return row, nil
	}
	// A defaulted id (Postgres DEFAULT gen_random_uuid(), serials): read it
	// back from the insert itself so children can reference it.
	var id any
	if err := db.QueryRowContext(ctx, q+` RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("%s RETURNING id: %w", q, err)
	}
	if b, ok := id.([]byte); ok {
		id = string(b)
	}
	row["id"] = id
	return row, nil
}

// wipeRefs maps each reference column of tb to (parent table, parent column):
// the driver's declared foreign keys, plus SQLiteCascadeGraph edges (which
// SQLite mostly does not declare).
func wipeRefs(tb *wipeTable, postgres bool) map[string][2]string {
	refs := map[string][2]string{}
	for _, fk := range tb.fks {
		refs[fk.col] = [2]string{fk.parent, fk.ref}
	}
	for _, e := range persistence.SQLiteCascadeGraph {
		if _, ok := refs[e.Column]; !ok && e.Child == tb.name && (postgres || !e.PostgresOnly) {
			refs[e.Column] = [2]string{e.Parent, "id"}
		}
	}
	return refs
}

var wipeSeedTime = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// wipeColumnValue decides one column's value, and whether to write it at all.
func wipeColumnValue(tb *wipeTable, c wipeCol, project string, refs map[string][2]string,
	parents map[string]map[string]any, postgres bool, fixed map[string]any) (any, bool) {
	if c.generated {
		return nil, false
	}
	if v, ok := fixed[c.name]; ok {
		return v, true
	}
	if v, ok := wipeValueOverrides[tb.name+"."+c.name]; ok {
		return v, true
	}
	if c.name == "project_id" {
		return project, true
	}
	if ref, ok := refs[c.name]; ok && parents[ref[0]][ref[1]] != nil {
		return parents[ref[0]][ref[1]], true
	}
	if (c.notNull || c.name == "id") && !c.hasDefault {
		return wipeValue(c, tb.name, wipeSeedTime, postgres), true
	}
	return nil, false
}

// wipeValue generates a value of c's type, unique where a uniqueness
// constraint could bite.
func wipeValue(c wipeCol, table string, now time.Time, postgres bool) any {
	n := wipeSeq.Add(1)
	t := c.typ
	switch {
	case strings.HasPrefix(t, "_"): // Postgres array
		return "{}"
	case strings.Contains(t, "bool"):
		return false
	case strings.Contains(t, "int") || strings.Contains(t, "serial"):
		return n
	case strings.Contains(t, "float") || strings.Contains(t, "real") || strings.Contains(t, "numeric") ||
		strings.Contains(t, "double") || strings.Contains(t, "decimal"):
		return float64(n % 1000)
	case strings.Contains(t, "timestamp") || t == "date":
		if postgres {
			return now
		}
		return persistence.SQLiteTime(now)
	case strings.Contains(t, "json"):
		return "{}"
	case t == "bytea" || strings.Contains(t, "blob"):
		return []byte("x")
	case t == "uuid":
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", n%1_000_000_000_000)
	case t == "inet":
		return "127.0.0.1"
	case t == "interval":
		return "1 hour"
	}
	if !postgres && (strings.HasSuffix(c.name, "_at") || c.name == "ts") {
		return persistence.SQLiteTime(now)
	}
	return fmt.Sprintf("%s-%s-%d", table, c.name, n)
}

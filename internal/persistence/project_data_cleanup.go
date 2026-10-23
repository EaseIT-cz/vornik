package persistence

import (
	"context"
	"strings"
)

// ProjectDataStats summarises what a wipe touched. Counts are
// per-table best-effort — backends that can't report affected
// rows (sqlite mock paths, etc.) return -1 for that table rather
// than failing the cleanup. Audit + UI surfaces tolerate -1 by
// rendering "unknown" instead of a misleading zero.
type ProjectDataStats struct {
	// TablesCleared is the count of project-scoped tables the
	// deleter ran a DELETE against. Equal to len(ProjectDataTablesFor(driver))
	// on the happy path. Lower when an early-table delete failed
	// and the transaction rolled back.
	TablesCleared int

	// RowsDeleted aggregates affected-row counts across every
	// table when the backend reports them (postgres RowsAffected).
	// -1 means at least one DELETE returned an unknown count;
	// callers should treat the number as a lower bound or
	// "unknown".
	RowsDeleted int64

	// CachedEmbeddingsEvicted counts embedding_cache rows removed for this
	// project. That table is keyed (content_hash, model) with no project_id
	// column, so it is unreachable by ProjectDataTables and is deleted by a
	// dedicated evictor instead.
	CachedEmbeddingsEvicted int

	// CacheEvictionRan says whether that evictor was wired and invoked at all.
	// Without it a zero could mean "nothing to evict" or "never attempted",
	// and an audit row showing the first while meaning the second is precisely
	// the control this codebase does not accept.
	CacheEvictionRan bool

	// CacheEvictionNotApplicable says the driver has no embedding_cache at
	// all (SQLite), so there was nothing derived to evict. It keeps "nothing
	// to evict here" distinct from "an evictor was never wired" (GitHub #365).
	CacheEvictionNotApplicable bool
}

// ProjectDataDeleter wipes every row in every project-scoped
// table for one project ID. Implementations run the work in a
// single transaction so a mid-way failure rolls back cleanly —
// the archived-project sweeper retries on its next tick rather
// than fighting a partial state.
//
// Does NOT delete:
//
//   - admin_audit_log: preserves the historical record of who
//     archived/deleted what. The action row references the
//     project ID as Target so investigators can trace deletions
//     even after the rest is gone.
//   - cross_project_calls where this project is *caller*: the
//     callee may still want to see the inbound edge for their
//     own replay tree. The CPC row's caller_project becomes a
//     dangling reference; the replay builder already tolerates
//     that.
//
// Returns ProjectDataStats describing the wipe scope so the
// sweeper can write a precise audit row.
type ProjectDataDeleter interface {
	DeleteProjectData(ctx context.Context, projectID string) (ProjectDataStats, error)
}

// ProjectDataTables enumerates every table whose rows are
// scoped to a project_id column and which should be wiped when
// the project is deleted. Order matters only for FKs that don't
// cascade — we run children before parents to be defensive even
// though most child tables have ON DELETE CASCADE on a parent
// (tasks/executions/artifacts).
//
// Update this list when a new project-scoped table lands, or name
// the table in ProjectScopedTablesNotWiped with a reason: a guard
// test per driver reads every project_id table from the live schema
// and fails on one in neither (GitHub #365). Tables SQLite lacks go
// in ProjectDataTablesPostgresOnly.
//
// Note: api_keys is intentionally included — once the project
// is gone its keys must stop authenticating too.
var ProjectDataTables = []string{
	// Trading lineage — fills cascade off orders so deleting
	// orders first keeps the FK contract tidy.
	"trading_fills",
	"trading_orders",
	"trading_positions_snapshots",
	"trading_safety_events",

	// Memory pipeline — child queues + DLQ first, then the
	// chunks they reference (project_memory_chunks has
	// CASCADE-from-chunk dependants, so deleting chunks
	// cascades to mention rows etc.).
	"memory_embed_queue",
	"memory_embed_dlq",
	"memory_eviction_audit",
	// The run headers those tombstones back-link to. AFTER the tombstones and
	// LOAD-BEARING: the FK is ON DELETE RESTRICT, so deleting a header while
	// its tombstones exist fails the whole wipe. Order is the contract here,
	// not tidiness — TestProjectDataTables_EvictionTombstonesPrecedeRuns pins
	// it.
	"memory_eviction_runs",
	"project_memory_quarantine",
	"project_memory_chunks",
	"project_ingest_queue",
	// Memory verification (migration 234, design 22 amendment #482): the
	// provenance records and the verification events of a deleted project's
	// chunks. memory_verification_backup goes with its events (cascade).
	// No content: ids, blob ids, states, counts, actor and reason.
	"memory_chunk_provenance",
	"memory_verification_events",

	// Memory audit trails (GitHub #370, operator decision 2026-10-14): they
	// hold content or query text of a deleted project. No foreign key reaches
	// or leaves any of them on either driver, so their place in the order is
	// free. The retention windows that bound them (memory_ingest_audit 90 d,
	// memory_policy_evaluations 30/365 d) govern live projects; deleting a
	// project is the explicit act the tool-audit retention floor (2026-10-12
	// design §1.3) already treats as not floor-relevant. memory_quarantine is
	// SQLite's quarantine table (ProjectDataTablesSQLiteOnly).
	"memory_ingest_audit",
	"memory_retrieval_audit",
	"memory_search_stages",
	"memory_policy_evaluations",
	"memory_quarantine",

	// Knowledge graph — entities cascade to edges + mentions.
	"knowledge_entities",

	// Corpus epoch ledger.
	"corpus_rollbacks",
	"corpus_epochs_active",
	"corpus_embedding_versions_active",
	"corpus_epochs",

	// Audit + analytics — task-scoped tables cascade off
	// tasks; project-scoped ones go via project_id.
	"task_judge_verdicts",
	"task_post_mortems",
	"task_llm_usage",
	"execution_step_outcomes",
	"chat_audit_log",
	"intent_verdicts",
	"tool_audit_log",
	"webhook_events",
	"autonomy_evaluations",

	// Workflow self-healing (blackbox): triggers + per-class
	// override rows are both project-scoped (project_id NOT NULL).
	// Without these, deleting a project orphans its open healing
	// triggers — they linger in /ui/admin/blackbox/triggers/ for a
	// workflow whose project no longer exists, and a stale override
	// keeps muting/retuning detection for the dead project. Neither
	// table FK-cascades, so an explicit project_id pass is the only
	// thing that wipes them.
	"workflow_healing_triggers",
	"workflow_healing_overrides",

	// Documents + per-project gist cache.
	"extracted_documents",
	"project_gists",

	// Dispatcher reminders (B-12, 2026-05-28): pending reminders
	// tied to a deleted project would otherwise fire after the
	// project's gone — operator sees a phantom Telegram/email
	// reminder from a project they can't navigate to anymore.
	// Nullable project_id means reminders with NULL project_id
	// (operator-set without a project context) survive — those
	// aren't tied to any specific project's lifecycle anyway.
	"dispatcher_reminders",

	// Agent-administration ledgers (migrations 229 and 226). Retention cannot
	// reach a deleted project's rows — it iterates only projects still in the
	// registry — so without these entries they outlived the project forever,
	// contrary to migration 229's comment and the agent-admin design §23.2
	// (audit finding, GitHub #365, 2026-10-13).
	"agent_admin_refusals",
	"agent_connector_installs",

	// Authorisations (archival design section I, #370 carry-over, delta
	// audit 2026-10-14). Project IDs are operator-chosen names: left behind,
	// a project re-created under the same ID inherited the deleted project's
	// standing broker grants (keyed project + workflow + action), its device-
	// approved workflow reach (keyed workflow_id, which embeds the project)
	// and its integration approvals (keyed project + integration). An
	// authorisation of a deleted project must not outlive it, as api_keys and
	// mcp_oauth_tokens do not. execution_tool_grants cannot be inherited
	// (keyed by a random execution id) but is the trail of executions this
	// wipe already removes. No foreign key reaches or leaves any of the four
	// on either driver.
	"broker_standing_grants",
	"agent_workflow_approvals",
	"agent_integration_approvals",
	"execution_tool_grants",

	// Archival design section J (GitHub #370, 2026-10-16; the W8 census
	// "UNREVIEWED 16"). Every remaining project table decided. Wholly wiped:
	// group_projects is an authorisation (a group's grant on a deleted
	// project must not open a re-created one); budget_reservations,
	// forge_*, instincts and project_first_seen were inherited by a project
	// re-created under the same ID; the rest hold content or the trail of
	// executions this wipe removes. instincts' four children cascade
	// (SQLiteCascadeGraph on SQLite); global instincts carry project_id ''
	// and never match.
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
	// Partly wiped (ProjectDataTablesPartial): the project's own skills, not
	// its global ones; its project-radius proposals, with the others
	// scrubbed of free text.
	"project_skills",
	"control_plane_proposals",

	// project_wizard_sessions is intentionally NOT included:
	// the table is operator-scoped (committed_project_id is the
	// only project linkage, and uncommitted sessions have no
	// project linkage at all). A future iteration may add a
	// `DELETE WHERE committed_project_id = $1` pass; today the
	// session rows stay so the operator's wizard transcript
	// history outlives any single project's lifecycle.
	//
	// telegram_task_threads is intentionally NOT included: it
	// cascades via task_id ON DELETE CASCADE when `tasks` is
	// wiped below. Explicit DELETE would error on the missing
	// project_id column.

	// Artifacts row (the blob file is deleted out of band by
	// the sweeper's filesystem step).
	"artifacts",

	// Core tasks/executions last so the cascades into
	// task-scoped tables (task_messages, task_watchers,
	// execution_hints, etc.) run after their parent rows.
	"executions",
	"tasks",

	// API keys — scoped to the project.
	"api_keys",

	// MCP OAuth access/refresh tokens of the project's connectors: credentials
	// of a deleted project, removed on the reasoning that removes api_keys
	// (GitHub #370, operator decision 2026-10-14). Not revoked at the vendor:
	// the wipe deletes the local copy only.
	"mcp_oauth_tokens",

	// LAST, and load-bearing: the GDPR index loses the links whose row this
	// wipe removed, and its predicate asks whether the linked row still
	// exists — true only once every table above is wiped (task_messages goes
	// with tasks). Partly wiped; see ProjectDataTablesPartial. Section J.
	"data_subject_links",
}

// DataSubjectLinkTablesWiped names the linkable tables of the subject axis
// (datasubject.LinkableTables) whose rows the project wipe deletes — listed
// above, or (task_messages) a cascade child of a listed table. The wipe
// removes a data_subject_links row pointing into one of them once the row is
// gone: erasure's rule is that a link goes with its row (datasubject.go,
// subjectstore.DeleteRow). internal/datasubject's
// TestDataSubjectLinkTablesWiped_MatchesTheWipe pins the list to the wipe.
// Every one has a TEXT id that a link's row_id names.
//
// see LLD § https://docs.vornik.io "2026-10-16 — every project table decided"
var DataSubjectLinkTablesWiped = []string{
	"artifacts",
	"chat_audit_log",
	"extracted_documents",
	"knowledge_entities",
	"project_memory_chunks",
	"task_messages",
}

// PartialWipe is how the wipe treats a table whose project rows it removes
// only in part.
type PartialWipe struct {
	// Where is AND-ed to the project match of the DELETE: the rows it matches
	// go. SQL text, a package constant with no placeholder, valid on both
	// drivers.
	Where string
	// Keeps says which of the project's rows survive, and why.
	Keeps string
	// Scrub, when set, is a SET list (no placeholder) applied in the same
	// transaction, before the delete, to the project's rows Where does not
	// match: the kept rows lose the free text that could quote the project.
	Scrub string
	// MatchUnscoped widens the project match to rows with no project_id
	// (Where must then select only rows that are provably this wipe's to
	// remove).
	MatchUnscoped bool
}

// ProjectDataTablesPartial names the tables of ProjectDataTables the wipe
// removes only in part (archival design section J, GitHub #370). Each is
// pinned by persistence.TestProjectDataTablesPartial_Pinned and seeded with a
// remove row and a keep row by the repotest wipe suite.
//
// see LLD § https://docs.vornik.io "2026-10-16 — every project table decided"
var ProjectDataTablesPartial = map[string]PartialWipe{
	"project_skills": {
		Where: "NOT is_global",
		Keeps: "global skills (is_global): the operator promoted them to inject into every project's roles; " +
			"deleting the authoring project must not silently remove know-how other projects use",
	},
	"control_plane_proposals": {
		Where: "blast_radius = 'project'",
		Keeps: "proposals whose blast radius (model, swarm, daemon) outlives the project: an applied one's " +
			"pre_apply_snapshot is its only rollback and the row is its approval record; title, rationale and " +
			"evidence are scrubbed because they can quote the project's tasks",
		Scrub: "title = 'proposal of deleted project ' || project_id, rationale = '', evidence = ''",
	},
	"data_subject_links": {
		Where: dataSubjectLinksGone(),
		Keeps: "links to rows the wipe does not remove (channel_sessions, operator_profile, user_identities, " +
			"or a row that still exists): a later erasure must still find the person there",
		MatchUnscoped: true,
	},
}

// dataSubjectLinksGone builds the data_subject_links predicate: the link
// points into one of DataSubjectLinkTablesWiped and its row no longer exists.
func dataSubjectLinksGone() string {
	terms := make([]string, 0, len(DataSubjectLinkTablesWiped))
	for _, tb := range DataSubjectLinkTablesWiped {
		terms = append(terms, "(table_name = '"+tb+"' AND NOT EXISTS (SELECT 1 FROM "+tb+
			" x WHERE x.id = data_subject_links.row_id))")
	}
	return strings.Join(terms, " OR ")
}

// ProjectDataStatements returns the statements the wipe runs for table, in
// order, with ph the driver's placeholder for the one project-id argument
// each takes ("?" or "$1"): a Scrub UPDATE when the table is partly wiped and
// scrubs its kept rows, then the DELETE.
func ProjectDataStatements(table, ph string) []string {
	match := "project_id = " + ph
	pw, partial := ProjectDataTablesPartial[table]
	if !partial {
		return []string{"DELETE FROM " + table + " WHERE " + match}
	}
	var out []string
	if pw.Scrub != "" {
		out = append(out, "UPDATE "+table+" SET "+pw.Scrub+" WHERE "+match+" AND NOT ("+pw.Where+")")
	}
	if pw.MatchUnscoped {
		match = "(" + match + " OR project_id IS NULL)"
	}
	return append(out, "DELETE FROM "+table+" WHERE "+match+" AND ("+pw.Where+")")
}

// ProjectDataTablesPostgresOnly names the tables of ProjectDataTables the
// SQLite schema does not have. The SQLite wipe skips them; the SQLite guard
// test (sqlite/project_data_cleanup_test.go) fails if one ever appears there,
// so the flag cannot go stale, and fails if any other listed table is missing.
//
// GitHub #365 (2026-10-13): the wipe ran this list unfiltered on SQLite and
// failed at memory_embed_queue, rolling back and deleting nothing.
// see LLD § https://docs.vornik.io "2026-10-13 — the wipe on SQLite"
var ProjectDataTablesPostgresOnly = map[string]bool{
	"memory_embed_queue":               true,
	"memory_embed_dlq":                 true,
	"memory_eviction_audit":            true,
	"memory_eviction_runs":             true,
	"project_memory_quarantine":        true,
	"corpus_embedding_versions_active": true,
	"workflow_healing_triggers":        true,
	"workflow_healing_overrides":       true,
	"project_gists":                    true,
	"dispatcher_reminders":             true,
	// Section J (GitHub #370): two of the sixteen exist only on Postgres.
	"blackbox_trace_cache": true,
	"web_write_actions":    true,
}

// ProjectDataTablesSQLiteOnly names the tables of ProjectDataTables the
// Postgres schema does not have; the Postgres wipe skips them and the Postgres
// guard (integration lane) fails if one ever appears there.
//
// GitHub #370 (2026-10-14): memory_quarantine is SQLite's own quarantine
// table (Postgres has project_memory_quarantine, a different table).
// see LLD § https://docs.vornik.io "2026-10-14 — six tables move into the wipe"
var ProjectDataTablesSQLiteOnly = map[string]bool{
	"memory_quarantine": true,
}

// ProjectDataTablesFor returns ProjectDataTables, in order, as the driver
// runs it: on Postgres every table but the SQLiteOnly ones, on SQLite every
// table but the PostgresOnly ones.
func ProjectDataTablesFor(postgres bool) []string {
	out := make([]string, 0, len(ProjectDataTables))
	for _, t := range ProjectDataTables {
		if (postgres && !ProjectDataTablesSQLiteOnly[t]) || (!postgres && !ProjectDataTablesPostgresOnly[t]) {
			out = append(out, t)
		}
	}
	return out
}

// NotWipedCascadePrefix starts the reason of a table removed with its parent:
// by ON DELETE CASCADE on Postgres, by SQLiteCascadeGraph on SQLite. The SQLite
// guard checks that a table so excused, where it exists, has a graph edge from
// the named parent — otherwise the excuse would hide an orphan.
const NotWipedCascadePrefix = "removed with its parent (Postgres ON DELETE CASCADE; SQLiteCascadeGraph on SQLite): "

// NotWipedKeptPrefix starts the reason of a table whose project rows the wipe
// keeps by decision. The rest of the reason says why. The kept set is pinned
// by name (TestProjectScopedTablesNotWiped_EveryReasonDecided), so keeping a
// table's rows past its project is a reviewed decision, never a default.
// There is no third category: "UNREVIEWED" was retired by archival design
// section J (GitHub #370, 2026-10-16).
const NotWipedKeptPrefix = "kept: "

// ProjectScopedTablesNotWiped is the other half of the denominator: every
// table with a project_id column, on either driver, that ProjectDataTables
// does not list, with the reason. The guard tests (one per driver) read every
// base table with a project_id column from the live schema and fail on one in
// neither ProjectDataTables nor this map, so a new project-scoped table cannot
// be missed silently — it must be wiped, partly wiped
// (ProjectDataTablesPartial), or excluded here as a cascade child or with a
// "kept:" reason.
//
// see LLD § https://docs.vornik.io "2026-10-16 — every project table decided"
var ProjectScopedTablesNotWiped = map[string]string{
	// Removed by the cascade from a wiped parent (Postgres ON DELETE CASCADE;
	// SQLiteCascadeGraph on SQLite). They carry project_id but need no pass
	// of their own.
	"execution_narration":         NotWipedCascadePrefix + "executions",
	"execution_quality_scores":    NotWipedCascadePrefix + "executions",
	"knowledge_edges":             NotWipedCascadePrefix + "knowledge_entities",
	"workflow_healing_candidates": NotWipedCascadePrefix + "workflow_healing_triggers",
	"memory_verification_backup":  NotWipedCascadePrefix + "memory_verification_events",

	"secret_redaction_audit": NotWipedKeptPrefix + "the security record that a secret reached a sink in this " +
		"project, the evidence for rotating a credential that lives on at its vendor; it holds ids, a sink " +
		"name, a pattern name and a count, never the secret or any content (column set pinned by the guards)",
}

package persistence

import "context"

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

// notWipedUnreviewed is the reason for a project-scoped table nobody has yet
// decided the wipe should delete. Its rows outlive the project. The
// disposition is an operator decision (archival LLD 2026-10-13 §E).
const notWipedUnreviewed = "UNREVIEWED: left behind by the wipe; disposition open (archival LLD 2026-10-13 §E)"

// ProjectScopedTablesNotWiped is the other half of the denominator: every
// table with a project_id column, on either driver, that ProjectDataTables
// does not list, with the reason. The guard tests (one per driver) read every
// base table with a project_id column from the live schema and fail on one in
// neither ProjectDataTables nor this map, so a new project-scoped table cannot
// be missed silently — it must be wiped or excluded here with a reason.
var ProjectScopedTablesNotWiped = map[string]string{
	// Removed by the cascade from a wiped parent (Postgres ON DELETE CASCADE;
	// SQLiteCascadeGraph on SQLite). They carry project_id but need no pass
	// of their own.
	"execution_narration":         NotWipedCascadePrefix + "executions",
	"execution_quality_scores":    NotWipedCascadePrefix + "executions",
	"knowledge_edges":             NotWipedCascadePrefix + "knowledge_entities",
	"workflow_healing_candidates": NotWipedCascadePrefix + "workflow_healing_triggers",

	"blackbox_trace_cache":    notWipedUnreviewed,
	"broker_actions":          notWipedUnreviewed,
	"budget_reservations":     notWipedUnreviewed,
	"control_plane_proposals": notWipedUnreviewed,
	"data_subject_links":      notWipedUnreviewed,
	"fixit_sessions":          notWipedUnreviewed,
	"forge_ci_outcomes":       notWipedUnreviewed,
	"forge_pr_review_state":   notWipedUnreviewed,
	"group_projects":          notWipedUnreviewed,
	"instincts":               notWipedUnreviewed,
	"project_first_seen":      notWipedUnreviewed,
	"project_skills":          notWipedUnreviewed,
	"recovery_events":         notWipedUnreviewed,
	"secret_redaction_audit":  notWipedUnreviewed,
	"trading_fills_shadow":    notWipedUnreviewed,
	"web_write_actions":       notWipedUnreviewed,
}

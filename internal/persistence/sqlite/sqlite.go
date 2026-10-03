// Package sqlite provides a SQLite-backed implementation of the
// persistence-package repository interfaces. The backend is intended
// for local development and integration tests — production deploys
// stay on Postgres.
//
// SQLite-specific concessions documented in
// https://docs.vornik.io:
//
//   - Single-writer at the database level: all writes serialize.
//     Fine for tests + dev; not for multi-tenant production.
//   - No SKIP LOCKED: the lease query degrades to BEGIN IMMEDIATE +
//     a row-level update under a held transaction. Concurrent
//     LeaseTask callers serialize; the single-scheduler-goroutine
//     deployment shape we ship doesn't notice.
//   - TEXT arrays (`completed_steps TEXT[]`, etc.) → JSON-encoded
//     TEXT columns; helper sqliteStringArray drives the round-trip.
//   - Enum types → TEXT + CHECK constraint.
//   - `NOW()` → `datetime('now')` (UTC text).
//   - Placeholder style: SQLite accepts `?` only (no `$N`); each repo
//     ships its own SQL string distinct from the postgres sibling.
//
// WAL journal mode is enabled at Connect so concurrent reads don't
// block writes — critical for any test that touches the scheduler's
// lease path.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Config holds SQLite connection configuration. Mirrors
// internal/config.DatabaseConfig.Path; additional knobs are
// hard-coded sensibly for the test/dev use case.
type Config struct {
	// Path is the on-disk database file. Pass ":memory:" for an
	// ephemeral test DB; one connection only (SQLite in-memory
	// databases are not shared across connections by default).
	Path string

	// MaxOpenConns caps the connection pool. Defaults to 1 for
	// :memory: (otherwise the pool would create multiple
	// independent in-memory DBs) and 5 for file-backed paths.
	MaxOpenConns int

	// ConnectTimeout bounds the initial open + ping. Zero defers to the
	// caller's context deadline, or 5s when it has none (connectContext).
	ConnectTimeout time.Duration
}

// DefaultConfig returns a Config tuned for ephemeral test usage: an
// in-memory database on one connection, with the fixture bound. No production
// code calls it (storage.go builds its Config through sqliteConfig).
func DefaultConfig() Config {
	cfg := FixtureConfig(":memory:")
	cfg.MaxOpenConns = 1
	return cfg
}

// FixtureConnectTimeout bounds the open + ping of a test fixture's database.
// The daemon's bound is defaultConnectTimeout (5 s) and is not this. Tests do
// not measure connect latency; the bound exists so a wedged open fails the
// test, not the package's go test timeout. Under parallel `make test` lanes on
// a loaded disk the first open of a fresh file (WAL creation, fsync under
// synchronous(FULL)) took 5.8-6.7 s and failed fixtures bounded at 5 s
// (2026-09-23/24, three more on 2026-10-03). Storage-abstraction design,
// "SQLite test fixtures" (2026-10-03).
const FixtureConnectTimeout = 60 * time.Second

// FixtureConfig is the Config a test fixture opens path with: the fixture
// bound, and Connect's default pool for the path (1 for ":memory:", 5 for a
// file). It deliberately leaves MaxOpenConns zero: an explicit value would
// break the memory-vs-file split. Tests outside this package open through
// package sqlitetest.
func FixtureConfig(path string) Config {
	return Config{Path: path, ConnectTimeout: FixtureConnectTimeout}
}

// DB wraps the sql.DB plus configuration metadata for the
// storage.Backend integration. Mirrors postgres.DB for symmetry —
// callers that hold a *sqlite.DB pointer can call Migrate / Close /
// IsReady the same way they would on Postgres.
type DB struct {
	*sql.DB
	config Config
	// normalizeRecorder, when set, observes each timestamp column pass the
	// one-time normaliser runs during Migrate. Test seam; per instance, so
	// parallel tests cannot see each other's passes.
	normalizeRecorder func(tableColumn)
}

// Connect opens a SQLite database at cfg.Path, verifies connectivity,
// and applies the consolidated schema. Returns a ready-to-use *DB.
//
// Unlike Postgres there's no incremental migration history — the
// schema is single-version + idempotent (CREATE TABLE IF NOT EXISTS
// throughout) so multiple Connect calls on the same file converge.
// This trades the historical reproducibility of Postgres migrations
// for simpler test fixtures: every test starts from the latest schema
// with no migration ordering to worry about.
func Connect(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		cfg.Path = ":memory:"
	}
	// Create the DB file's parent directory if it's missing. SQLite
	// fails with SQLITE_CANTOPEN (error 14) rather than creating
	// missing parents — a common footgun for file-backed paths like
	// ./.dev/vornik.db. Skip the in-memory DB (no filesystem path).
	if cfg.Path != ":memory:" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite: create db dir %q: %w", dir, err)
			}
		}
	}
	if cfg.MaxOpenConns <= 0 {
		if cfg.Path == ":memory:" {
			cfg.MaxOpenConns = 1
		} else {
			cfg.MaxOpenConns = 5
		}
	}

	// The modernc.org/sqlite driver registers under the name
	// "sqlite". Pass file-mode pragmas via the connection string so
	// they apply on every fresh connection in the pool.
	// foreign_keys intentionally OFF for phase 2: only 4 of the 28
	// repos are implemented, so a write into artifacts.task_id (for
	// example) has no parent row in tasks yet. Once TaskRepository
	// + others land, flip this back to ON and seed parent rows in
	// the shared test setup.
	// synchronous(FULL) is the durability half of the config-apply journal
	// contract (2026-09-13 config-assistant review R6, plan §7g): the
	// journal's PREPARED row must be on disk before the first file write,
	// and under WAL the driver default (NORMAL) can lose the last committed
	// transactions on power loss. Set explicitly so the contract does not
	// depend on a driver default; storage.ProbeDurability reads it back.
	//
	// _txlock=immediate makes every read-write transaction BEGIN IMMEDIATE,
	// which LeaseTask, IngestQueue.ClaimBatch and the corpus epoch swap rely
	// on (read candidates, then write, with no concurrent writer between).
	// modernc.org/sqlite ignores TxOptions.Isolation, so asking for
	// LevelSerializable did nothing and they ran DEFERRED until 2026-10-02.
	// ReadOnly transactions stay DEFERRED and do not take the writer lock.
	dsn := cfg.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(OFF)&_pragma=synchronous(FULL)&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)

	pingCtx, cancel, bound := connectContext(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping %q (bound: %s): %w", cfg.Path, bound, err)
	}

	return &DB{DB: db, config: cfg}, nil
}

// defaultConnectTimeout bounds an open whose caller set neither a
// ConnectTimeout nor a context deadline — the daemon's case.
const defaultConnectTimeout = 5 * time.Second

// connectContext derives the context that bounds Connect's open + ping, and
// names the bound for the error message (storage-abstraction design, SQLite
// connect deadline, 2026-09-24). In order: a configured timeout (still capped
// by the caller, as any derived context is); else the caller's own deadline,
// with no second, shorter one imposed — the caller owns the budget; else the
// 5 s default. Before this, Connect always imposed 5 s, so `doctor --offline`'s
// 30 s and a saturated host's 6 s first open collided.
func connectContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, string) {
	if timeout > 0 {
		if d, ok := ctx.Deadline(); ok && time.Until(d) < timeout {
			// The caller's shorter deadline caps the derived context, so it is
			// the bound that fires — label it as such.
			c, cancel := context.WithCancel(ctx)
			return c, cancel, fmt.Sprintf("%s from caller deadline (under ConnectTimeout %s)", time.Until(d).Round(time.Millisecond), timeout)
		}
		c, cancel := context.WithTimeout(ctx, timeout)
		return c, cancel, fmt.Sprintf("%s from ConnectTimeout", timeout)
	}
	if d, ok := ctx.Deadline(); ok {
		c, cancel := context.WithCancel(ctx)
		return c, cancel, fmt.Sprintf("%s from caller deadline", time.Until(d).Round(time.Millisecond))
	}
	c, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	return c, cancel, fmt.Sprintf("%s default", defaultConnectTimeout)
}

// Migrate applies the consolidated schema. Idempotent.
func (d *DB) Migrate(ctx context.Context) error {
	// Additive columns FIRST. schemaSQL is CREATE TABLE IF NOT EXISTS, which
	// is a no-op on a table that already exists — so a column added to
	// schemaSQL after a database was created never lands there. That is
	// survivable for a plain column (queries error) but fatal when schemaSQL
	// also indexes it: the CREATE INDEX fails and Migrate returns an error,
	// so the daemon will not start at all against an existing database.
	// Reconciling before schemaSQL means the index has its column by the time
	// it is created.
	if err := d.applyAdditiveColumns(ctx); err != nil {
		return err
	}
	if err := d.applyTableRebuilds(ctx); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	// One-time data normalisation, after the schema so every column exists
	// (SQLite timestamp ordering design, D4).
	return d.applyTimestampNormalization(ctx)
}

// additiveColumn is a column added to an EXISTING table after that table first
// shipped in schemaSQL.
//
// Postgres gets these through numbered migrations; SQLite has no migration
// runner, only the idempotent starter schema, so they need this reconciler.
// Any future column added to an existing table in schemaSQL must be registered
// here too — otherwise it silently exists on fresh databases only.
type additiveColumn struct {
	table  string
	column string
	// ddl is the type + constraints, e.g. `TEXT NOT NULL DEFAULT ''`. SQLite's
	// ADD COLUMN requires a non-null default when the column is NOT NULL.
	ddl string
}

// sqliteAdditiveBackfills holds, per "table.column", a statement run once,
// right after that column is added to an existing table (never on a fresh
// database, which has no rows to backfill).
var sqliteAdditiveBackfills = map[string]string{
	// Postgres migration 203 — the companion push outbox (broker
	// write-actions design §7a). Existing rows are marked as already pushed
	// so an upgrade does not push a backlog: the task status for configs,
	// the §6 front state for actions: pending, and staged of a COMPLETED
	// task, are pending_approval; staged of an unfinished task stays NULL and
	// is pushed once when promoted (review-20260930-c6b5 F1).
	"a2a_push_configs.pushed_state": `UPDATE a2a_push_configs SET pushed_state = (SELECT status FROM tasks WHERE tasks.id = a2a_push_configs.task_id)`,
	"broker_actions.pushed_state": `UPDATE broker_actions SET pushed_state = CASE
		WHEN status = 'pending' THEN 'pending_approval'
		WHEN status = 'staged' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = broker_actions.task_id AND t.status = 'COMPLETED') THEN 'pending_approval'
		WHEN status = 'staged' THEN NULL
		ELSE status END`,
}

// sqliteAdditiveColumns is the registry. Keep it append-only and in the same
// order as the corresponding Postgres migrations, so the two backends can be
// diffed by eye.
var sqliteAdditiveColumns = []additiveColumn{
	// Postgres migration 154 — knowledge-skill dedup preflight (LLD §12.2).
	{"project_skills", "embedding", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "embedding_model", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "supersedes_id", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "distinct_justification", `TEXT NOT NULL DEFAULT ''`},
	// Postgres migration 175 — step-prompt persistence: hashes into step_prompts.
	{"execution_step_outcomes", "prompt_system_hash", `TEXT NOT NULL DEFAULT ''`},
	{"execution_step_outcomes", "container_memory_peak_bytes", `INTEGER`},
	{"execution_step_outcomes", "prompt_user_hash", `TEXT NOT NULL DEFAULT ''`},
	{"execution_step_outcomes", "prompt_tools_hash", `TEXT NOT NULL DEFAULT ''`},
	// Postgres migration 180 — which BODY of a skill an execution ran with
	// (LLD 2026-09-08-execution-ratings-approval-paths-design §4). NULLABLE on
	// purpose: a row written before this has an unknowable body, and a
	// non-null default would make every historical row look like a match for
	// whatever body is being approved today.
	{"execution_injected_skills", "body_sha256", `TEXT`},
	// Postgres migration 182 — when Forge commented on a CI run
	// (LLD 2026-09-08-forge-ci-outcomes-design §13.6). Nullable: NULL means
	// "not yet commented", which is the state every existing row is in.
	{"forge_ci_outcomes", "commented_at", `TEXT`},
	// Postgres migration 183 — deliberate access revocation marker (2026-09-13 R3).
	// Nullable: NULL = never deliberately revoked, which every existing row is.
	{"users", "access_revoked_at", `TEXT`},
	// Postgres migration 185 — proposal identifier + actor columns
	// (config-assistant plan §7g). Nullable, no backfill: a legacy row has no
	// request, entrypoint or resolved actor. schemaSQL indexes idempotency_key, so
	// these MUST land before schemaSQL runs (Migrate orders it so).
	{"control_plane_proposals", "request_id", `TEXT`},
	{"control_plane_proposals", "idempotency_key", `TEXT`},
	{"control_plane_proposals", "entrypoint", `TEXT`},
	{"control_plane_proposals", "actor_kind", `TEXT`},
	{"control_plane_proposals", "actor_account_id", `TEXT`},
	{"control_plane_proposals", "actor_credential_id", `TEXT`},
	// Postgres migration 190 — recurring-series membership on memory chunks
	// (2026-09-16-retrieval-recency-design.md §6). Nullable, no backfill: NULL
	// means "not part of a series", which every existing row is. schemaSQL
	// creates idx_memory_chunks_series over this column, so on a database that
	// predates it the column MUST land here first — otherwise the CREATE INDEX
	// errors and Migrate fails the daemon's startup rather than one query.
	{"project_memory_chunks", "series_key", `TEXT`},
	// Member identity and TTL for the same re-rank (design §5.3/§5.1). These
	// are NOT new Postgres migrations — artifact_id, source_name and
	// expires_at have been on the production table for a long time; the slim
	// sqlite schema simply never carried them because nothing on this lane
	// read them. The recency predicate does, on both lanes, so they land here
	// too. Nullable with no backfill: NULL artifact_id is the case the
	// predicate's COALESCE exists for, and NULL expires_at is "no TTL", which
	// the curve reads as "never decays".
	{"project_memory_chunks", "source_name", `TEXT`},
	{"project_memory_chunks", "artifact_id", `TEXT`},
	{"project_memory_chunks", "expires_at", `TEXT`},
	// Postgres migration 200 — a document ingest's path in its repository
	// (memory rollback x supersession design, amendment 2026-09-26). Nullable,
	// no backfill: NULL means "not a document ingest".
	{"project_ingest_queue", "document_path", `TEXT`},
	{"api_keys", "delegate_disabled", `INTEGER NOT NULL DEFAULT 0`},
	// schemaSQL indexes project_memory_chunks(project_id, repo_scope,
	// source_name) for the same migration. repo_scope has been in the slim
	// chunk table since 2026-06-05; a database created before that lacks it,
	// and the CREATE INDEX would fail startup, so it lands here first.
	{"project_memory_chunks", "repo_scope", `TEXT`},
	// Postgres migration 203 — the companion push outbox; backfilled once
	// (sqliteAdditiveBackfills).
	{"a2a_push_configs", "pushed_state", `TEXT`},
	{"broker_actions", "pushed_state", `TEXT`},
	// Postgres migration 207 — the approval apply lease (agent-administered
	// Vornik design §9.2; review 20261002-4de8 F2).
	{"agent_approval_requests", "apply_holder", `TEXT`},
	{"agent_approval_requests", "apply_lease_until", `TEXT`},
	{"agent_approval_requests", "apply_attempts", `INTEGER NOT NULL DEFAULT 0`},
	// Postgres migration 208 — the agent admin key (design §5).
	{"api_keys", "agent_admin", `INTEGER NOT NULL DEFAULT 0`},
	{"api_keys", "agent_namespace", `TEXT NOT NULL DEFAULT ''`},
	// Postgres migration 209 — a permanently failed approved change.
	{"agent_approval_requests", "apply_error", `TEXT`},
}

// applyAdditiveColumns adds any registered column missing from an existing
// table. Tables that don't exist yet are skipped — schemaSQL creates them with
// the column already present, so a fresh database needs nothing here.
func (d *DB) applyAdditiveColumns(ctx context.Context) error {
	for _, c := range sqliteAdditiveColumns {
		exists, err := d.tableExists(ctx, c.table)
		if err != nil {
			return err
		}
		if !exists {
			continue // fresh database; schemaSQL will create it complete
		}
		has, err := d.columnExists(ctx, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		// SQLite has no ADD COLUMN IF NOT EXISTS, hence the check above.
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: add column %s.%s: %w", c.table, c.column, err)
		}
		if backfill := sqliteAdditiveBackfills[c.table+"."+c.column]; backfill != "" {
			if _, err := d.ExecContext(ctx, backfill); err != nil {
				return fmt.Errorf("sqlite: backfill %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	return nil
}

// tableRebuild brings an EXISTING table's definition up to date where SQLite
// cannot ALTER it in place: a CHECK constraint. schemaSQL is CREATE TABLE IF
// NOT EXISTS, so a changed CHECK otherwise reaches fresh databases only, and
// an existing one keeps refusing rows the code now writes.
//
// The rebuild runs when the table's stored definition lacks marker, a
// substring only the new definition contains. It creates the new table
// beside the old one, copies every row, drops the old table and renames the
// new one, in ONE transaction; schemaSQL then recreates the indexes. It is
// idempotent: afterwards the marker is present.
//
// It assumes a LEAF table: nothing references it by foreign key. Dropping a
// referenced table would break those references; such an entry must handle
// them explicitly. execution_quality_scores is a leaf, and its own reference
// to executions is re-declared by the DDL.
type tableRebuild struct {
	table  string
	marker string
	ddl    string // the table's CREATE TABLE statement, as in schemaSQL
}

// sqliteTableRebuilds is the registry, append-only. Postgres gets the same
// change through a numbered migration, named in each entry.
var sqliteTableRebuilds = []tableRebuild{
	// Postgres migration 199: execution_quality_scores learns `unscorable`
	// (agent-quality-benchmark design, amendment 2026-09-26).
	{"execution_quality_scores", "'unscorable'", executionQualityScoresTableSQL},
	// Postgres migration 211: agent_approval_requests learns `broker_action`
	// (agent-administered Vornik plan P4.8).
	{"agent_approval_requests", "'broker_action'", agentApprovalRequestsTableSQL},
	// Postgres migration 212: agent_approval_requests learns `host_action`
	// and decided_choice (Hermes approval transport design §4.1, §4.2). The
	// rebuild copies the old table's columns by name; decided_choice starts
	// NULL on every existing row.
	{"agent_approval_requests", "'host_action'", agentApprovalRequestsTableSQL},
}

func (d *DB) applyTableRebuilds(ctx context.Context) error {
	for _, r := range sqliteTableRebuilds {
		var def string
		err := d.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, r.table).Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			continue // fresh database; schemaSQL creates it current
		}
		if err != nil {
			return fmt.Errorf("sqlite: probe table %s: %w", r.table, err)
		}
		if strings.Contains(def, r.marker) {
			continue
		}
		if err := d.rebuildTable(ctx, r); err != nil {
			return fmt.Errorf("sqlite: rebuild %s: %w", r.table, err)
		}
	}
	return nil
}

func (d *DB) rebuildTable(ctx context.Context, r tableRebuild) error {
	tmp := r.table + "_rebuild"
	createTmp := strings.Replace(r.ddl, "CREATE TABLE IF NOT EXISTS "+r.table+" (", "CREATE TABLE "+tmp+" (", 1)
	if createTmp == r.ddl {
		return fmt.Errorf("DDL does not create %s", r.table)
	}
	cols, err := d.tableColumns(ctx, r.table)
	if err != nil {
		return err
	}
	// Named columns, not SELECT *: an ADD COLUMN puts a column last, so the
	// old table's order need not match the DDL's.
	list := strings.Join(cols, ", ")
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ` + tmp,
		createTmp,
		`INSERT INTO ` + tmp + ` (` + list + `) SELECT ` + list + ` FROM ` + r.table,
		`DROP TABLE ` + r.table,
		`ALTER TABLE ` + tmp + ` RENAME TO ` + r.table,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(stmt, "(", 2)[0], err)
		}
	}
	return tx.Commit()
}

func (d *DB) tableColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, `"`+c+`"`)
	}
	return cols, rows.Err()
}

func (d *DB) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: probe table %s: %w", table, err)
	}
	return n > 0, nil
}

func (d *DB) columnExists(ctx context.Context, table, column string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: probe column %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// IsReady checks that the connection is alive and the schema has
// landed. The "schema present" check is a SELECT against the
// migrations table sentinel — mirrors postgres.DB.IsReady's shape.
func (d *DB) IsReady(ctx context.Context) error {
	if err := d.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	var one int
	if err := d.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='tasks' LIMIT 1").Scan(&one); err != nil {
		return fmt.Errorf("sqlite: schema not applied: %w", err)
	}
	return nil
}

// Close closes the underlying connection pool.
func (d *DB) Close() error {
	return d.DB.Close()
}

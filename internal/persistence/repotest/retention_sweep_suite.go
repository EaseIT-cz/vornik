package repotest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/retention"
)

// RunRetentionSweepSuite pins the retention sweeper's statements against a
// REAL migrated database on either driver. The sweeper builds its own SQL
// (internal/retention), so the repository suites do not cover it; sqlmock
// checks statement text, not what a predicate selects.
//
// seedTime renders a timestamp the way the driver stores it: the time.Time
// itself on Postgres, persistence.SQLiteTime on SQLite. The suite seeds with
// raw SQL, so it must write what the driver's own writers write — otherwise it
// would test a form production never stores.
//
// The sweeper clock is FIXED (retention.WithClock) so a row can sit on the
// threshold's own UTC date. The suite asserts only on rows it created: the
// global tables are shared by every test on an integration database.
//
// audit 2026-10-13 (GitHub #360): on SQLite the sweeper bound thresholds as
// time.Time, which the driver renders "2026-04-16 01:02:03.5 +0000 UTC" against
// stored "2026-04-16T01:02:03.500000000Z", so every row on the threshold's own
// date read as not-yet-due; every global step failed at to_regclass; the
// artifact delete bound a Postgres array.
//
// see LLD § https://docs.vornik.io §4
func RunRetentionSweepSuite(t *testing.T, db *sql.DB, seedTime func(time.Time) any) {
	t.Helper()
	f := &retentionFixture{t: t, db: db, seedTime: seedTime, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	t.Run("project steps: tool_audit_log floor, firewall horizons, artifacts", func(t *testing.T) {
		f.t = t
		f.projectSteps()
	})
	t.Run("global steps: ratings, apply journal, link codes", func(t *testing.T) {
		f.t = t
		f.globalSteps()
	})
	t.Run("derived copies: blackbox_trace_cache, project_gists (#394)", func(t *testing.T) {
		f.t = t
		f.derivedCopies()
	})
}

type retentionFixture struct {
	t        *testing.T
	db       *sql.DB
	seedTime func(time.Time) any
	now      time.Time
	// expect is every seeded row and whether the sweep must keep it.
	expect []expectedRow
}

type expectedRow struct {
	table, idCol, id string
	keep             bool
}

func (f *retentionFixture) sweeper() *retention.Sweeper {
	return retention.New(f.db, zerolog.Nop(), retention.WithClock(func() time.Time { return f.now }))
}

func (f *retentionFixture) daysAgo(d int) time.Time { return f.now.AddDate(0, 0, -d) }

// seed inserts one row and records whether the sweep must keep it.
func (f *retentionFixture) seed(table, idCol, id string, keep bool, q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(context.Background(), q, args...); err != nil {
		f.t.Fatalf("seed %s: %v\n%s", table, err, q)
	}
	f.expect = append(f.expect, expectedRow{table: table, idCol: idCol, id: id, keep: keep})
}

// verify checks every seeded row's survival, then forgets them.
func (f *retentionFixture) verify() {
	f.t.Helper()
	for _, r := range f.expect {
		var n int
		if err := f.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM `+r.table+` WHERE `+r.idCol+` = $1`, r.id).Scan(&n); err != nil {
			f.t.Fatalf("count %s: %v", r.table, err)
		}
		if got := n == 1; got != r.keep {
			f.t.Errorf("%s %s: present=%v, want %v", r.table, r.id, got, r.keep)
		}
	}
	f.expect = nil
}

func (f *retentionFixture) projectSteps() {
	t := f.t
	ctx := context.Background()
	project := uniqueID("ret-proj")
	// A hand-built 30-day tool-audit policy, not through Resolve: the sweeper
	// must still apply the six-month floor (Art 26(6)).
	policy := retention.Policy{ProjectID: project, ToolAuditDays: 30,
		TaskLLMUsageDays: 90, TasksDays: 60, ExecutionsDays: 60, ArtifactsDays: 60, ChatAuditDays: 90,
		MemoryIngestAuditDays: 90, MemoryPolicyEvalAllowDays: 30, MemoryPolicyEvalBlockDays: 365,
		MemoryEvictionAuditDays: 365}
	floor := f.daysAgo(retention.ToolAuditFloorDays)

	audit := func(name string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("tool_audit_log", "id", id, keep, `INSERT INTO tool_audit_log
			(id, project_id, task_id, execution_id, tool_name, created_at)
			VALUES ($1, $2, 'task-x', 'exec-x', 'bash', $3)`, id, project, f.seedTime(at))
	}
	audit("ta-29d", f.daysAgo(29), true)
	audit("ta-179d", f.daysAgo(179), true)
	// Same UTC date as the floor threshold, one hour either side of it.
	audit("ta-thr-plus-1h", floor.Add(time.Hour), true)
	audit("ta-thr-minus-1h", floor.Add(-time.Hour), false)
	audit("ta-181d", f.daysAgo(181), false)
	audit("ta-400d", f.daysAgo(400), false)

	// GitHub #172 (2026-10-13): agent_admin_refusals follows the tool-audit
	// window, floor included (retention design §5).
	refusal := func(name string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("agent_admin_refusals", "id", id, keep, `INSERT INTO agent_admin_refusals
			(id, project_id, namespace, key_id, verb, rule, created_at)
			VALUES ($1, $2, 'retns', 'akey-x', 'add_api', 'url_not_https', $3)`, id, project, f.seedTime(at))
	}
	refusal("aar-29d", f.daysAgo(29), true)
	refusal("aar-thr-plus-1h", floor.Add(time.Hour), true)
	refusal("aar-thr-minus-1h", floor.Add(-time.Hour), false)
	refusal("aar-400d", f.daysAgo(400), false)

	evaluation := func(name, decision string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("memory_policy_evaluations", "id", id, keep, `INSERT INTO memory_policy_evaluations
			(id, project_id, chunk_id, decision, evaluated_at) VALUES ($1, $2, 'chunk-x', $3, $4)`,
			id, project, decision, f.seedTime(at))
	}
	evaluation("ev-allow-29d", "allow", f.daysAgo(29), true)
	evaluation("ev-allow-31d", "allow", f.daysAgo(31), false)
	evaluation("ev-allow-thr-minus-1h", "allow", f.daysAgo(30).Add(-time.Hour), false)
	evaluation("ev-block-364d", "block_expired", f.daysAgo(364), true)
	evaluation("ev-block-366d", "block_expired", f.daysAgo(366), false)

	artifact := func(name string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("artifacts", "id", id, keep, `INSERT INTO artifacts
			(id, project_id, name, artifact_class, storage_path, created_at)
			VALUES ($1, $2, 'a.txt', 'OUTPUT', '', $3)`, id, project, f.seedTime(at))
	}
	artifact("art-young", f.daysAgo(10), true)
	artifact("art-old", f.daysAgo(61), false)
	artifact("art-old2", f.daysAgo(400), false)

	preview, err := f.sweeper().Preview(ctx, policy)
	if err != nil {
		t.Errorf("Preview: %v", err)
	}
	swept, err := f.sweeper().Sweep(ctx, policy)
	if err != nil {
		t.Errorf("Sweep: %v", err)
	}
	for _, c := range []struct {
		name           string
		preview, swept int
		want           int
	}{
		{"tool_audit_log", preview.ToolAudit, swept.ToolAudit, 3},
		{"agent_admin_refusals", preview.AgentAdminRefusals, swept.AgentAdminRefusals, 2},
		{"allow evaluations", preview.MemoryPolicyEvalAllow, swept.MemoryPolicyEvalAllow, 2},
		{"block evaluations", preview.MemoryPolicyEvalBlock, swept.MemoryPolicyEvalBlock, 1},
		{"artifacts", preview.Artifacts, swept.Artifacts, 2},
	} {
		if c.preview != c.want || c.swept != c.want {
			t.Errorf("%s: preview %d, swept %d, want %d and %d", c.name, c.preview, c.swept, c.want, c.want)
		}
	}
	f.verify()
}

func (f *retentionFixture) globalSteps() {
	t := f.t
	ctx := context.Background()

	rating := func(name string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("execution_ratings", "execution_id", id, keep, `INSERT INTO execution_ratings
			(execution_id, rater_id, verdict, created_at, updated_at) VALUES ($1, 'rater', 'up', $2, $2)`,
			id, f.seedTime(at))
	}
	ratingsThreshold := f.daysAgo(retention.DefaultExecutionRatingsDays)
	rating("rate-young", f.daysAgo(399), true)
	rating("rate-old", f.daysAgo(401), false)
	// Same UTC date as the 400-day threshold: the global steps bound
	// time.Time too (review-20261013-fa9b finding 1).
	rating("rate-thr-minus-1h", ratingsThreshold.Add(-time.Hour), false)
	rating("rate-thr-plus-1h", ratingsThreshold.Add(time.Hour), true)

	journal := func(name, state string, terminal any, keep bool) {
		id := uniqueID(name)
		f.seed("config_apply_journal", "id", id, keep, `INSERT INTO config_apply_journal
			(id, proposal_id, state, op_digest, schema_version, deployment_root, affected_projects,
			 writer_epoch, ops, pre_images, read_set, created_at, updated_at, terminal_at)
			VALUES ($1, 'prop', $2, 'digest', 1, '/x', '[]', 1, '[]', '[]', '[]', $3, $3, $4)`,
			id, state, f.seedTime(f.daysAgo(100)), terminal)
	}
	journal("j-terminal-old", "APPLIED", f.seedTime(f.daysAgo(31)), false)
	journal("j-terminal-young", "APPLIED", f.seedTime(f.daysAgo(29)), true)
	journal("j-drift-old", "DRIFT", f.seedTime(f.daysAgo(90)), true)
	journal("j-open-old", "APPLYING", nil, true)

	user := uniqueID("ret-user")
	if _, err := f.db.ExecContext(ctx, `INSERT INTO users (id, display_name, created_at) VALUES ($1, 'Retention', $2)`,
		user, f.seedTime(f.daysAgo(100))); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	code := func(name string, expires time.Time, used any, keep bool) {
		id := uniqueID(name)
		f.seed("link_codes", "code_hash", id, keep, `INSERT INTO link_codes
			(code_hash, user_id, created_at, expires_at, used_at) VALUES ($1, $2, $3, $4, $5)`,
			id, user, f.seedTime(f.daysAgo(20)), f.seedTime(expires), used)
	}
	// `$1` appears twice in the link-code predicate.
	code("code-expired-old", f.daysAgo(10), nil, false)
	code("code-used-old", f.now.Add(time.Hour), f.seedTime(f.daysAgo(10)), false)
	code("code-expired-recent", f.daysAgo(3), nil, true)
	code("code-fresh", f.now.Add(time.Hour), nil, true)

	if _, err := f.sweeper().PreviewGlobal(ctx, retention.GlobalPolicy{}); err != nil {
		t.Errorf("PreviewGlobal: %v", err)
	}
	if _, err := f.sweeper().SweepGlobal(ctx, retention.GlobalPolicy{}); err != nil {
		t.Errorf("SweepGlobal: %v", err)
	}
	f.verify()
}

// derivedCopies pins the two derived-copy horizons of GitHub #394 (retention
// design §8.2.1, §8.2.2), on the compiled defaults (zero GlobalPolicy): a
// blackbox trace older than one day and a gist not refreshed for seven days
// are deleted, and a narrative not rewritten for seven days is cleared under
// a term row that stays.
//
// Found at: review (the Art 17 copies inventory, design 2026-07-29 §15.11.2);
// observed at integration — the rows outlived every source they were derived
// from because nothing deleted them.
//
// Both tables are Postgres-only. On SQLite the steps must be no-ops that report
// zero examined rather than errors.
//
// see LLD § https://docs.vornik.io §8
func (f *retentionFixture) derivedCopies() {
	t := f.t
	ctx := context.Background()
	sw := f.sweeper()

	var present bool
	if err := f.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE name = 'blackbox_trace_cache')`).Scan(&present); err == nil {
		// SQLite: neither table exists, so neither step may fail or count.
		g, err := sw.SweepGlobal(ctx, retention.GlobalPolicy{})
		if err != nil {
			t.Fatalf("SweepGlobal on SQLite: %v", err)
		}
		if present || g.BlackboxTraceCacheExamined != 0 || g.ProjectGistsExamined != 0 ||
			g.BlackboxTraceCache != 0 || g.ProjectGists != 0 || g.ProjectGistNarratives != 0 {
			t.Fatalf("SQLite has no derived-copy tables; got present=%v counts=%+v", present, g)
		}
		return
	}

	trace := func(name string, at time.Time, keep bool) {
		id := uniqueID(name)
		f.seed("blackbox_trace_cache", "task_id", id, keep, `INSERT INTO blackbox_trace_cache
			(task_id, project_id, trace_digest, schema_ver, payload, assembled_at, expires_at)
			VALUES ($1, 'ret-bb', 'digest', 1, '{"events":["a transcript line"]}', $2, $3)`,
			id, f.seedTime(at), f.seedTime(at.Add(24*time.Hour)))
	}
	trace("bb-2d", f.daysAgo(2), false)
	trace("bb-25h", f.now.Add(-25*time.Hour), false)
	trace("bb-23h", f.now.Add(-23*time.Hour), true)
	trace("bb-1h", f.now.Add(-time.Hour), true)

	gist := func(name string, generated time.Time, narrativeAt any, keep bool) string {
		id := uniqueID(name)
		f.seed("project_gists", "project_id", id, keep, `INSERT INTO project_gists
			(project_id, terms_json, chunks_scanned, generated_at, duration_ms,
			 narrative, narrative_model, narrative_generated_at)
			VALUES ($1, '[{"term":"alice","count":3}]', 3, $2, 1, 'Alice wrote about her appointment.', 'm', $3)`,
			id, f.seedTime(generated), narrativeAt)
		return id
	}
	gist("gist-8d", f.daysAgo(8), f.seedTime(f.daysAgo(8)), false)
	gist("gist-6d", f.daysAgo(6), f.seedTime(f.daysAgo(6)), true)
	staleNarrative := gist("gist-fresh-stale-narrative", f.now.Add(-10*time.Minute), f.seedTime(f.daysAgo(8)), true)
	freshNarrative := gist("gist-fresh-fresh-narrative", f.now.Add(-10*time.Minute), f.seedTime(f.now.Add(-time.Hour)), true)

	pre, err := sw.PreviewGlobal(ctx, retention.GlobalPolicy{})
	if err != nil {
		t.Fatalf("PreviewGlobal: %v", err)
	}
	// Denominators (CLAUDE.md §4): every seeded row was examined.
	if pre.BlackboxTraceCacheExamined < 4 || pre.ProjectGistsExamined < 4 {
		t.Errorf("examined counts must include the seeded rows: %+v", pre)
	}
	if pre.BlackboxTraceCache < 2 || pre.ProjectGists < 1 || pre.ProjectGistNarratives < 1 {
		t.Errorf("preview must count the due rows: %+v", pre)
	}
	g, err := sw.SweepGlobal(ctx, retention.GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if g.BlackboxTraceCache < 2 || g.ProjectGists < 1 || g.ProjectGistNarratives < 1 {
		t.Errorf("sweep must report what it removed: %+v", g)
	}
	f.verify()

	narrative := func(id string) (string, bool) {
		var n sql.NullString
		if err := f.db.QueryRowContext(ctx, `SELECT narrative FROM project_gists WHERE project_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read narrative %s: %v", id, err)
		}
		return n.String, n.Valid
	}
	if text, ok := narrative(staleNarrative); ok {
		t.Errorf("a narrative not rewritten for 7 days must be cleared, still %q", text)
	}
	if _, ok := narrative(freshNarrative); !ok {
		t.Errorf("a narrative rewritten an hour ago must be kept")
	}
	_, _ = f.db.ExecContext(ctx, `DELETE FROM project_gists WHERE project_id IN ($1, $2)`, staleNarrative, freshNarrative)
}

package repotest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/retention"
)

// CascadeDeleteRepos are the repositories whose deletes the cascade suite
// drives. Parents are created through them so the suite writes what the
// driver's own writers write.
type CascadeDeleteRepos struct {
	Tasks      persistence.TaskRepository
	Executions persistence.ExecutionRepository
	Artifacts  persistence.ArtifactRepository
}

// RunCascadeDeleteSuite pins what a parent delete does to its children, on a
// REAL migrated database. Every edge of persistence.SQLiteCascadeGraph is
// seeded (PostgresOnly edges only when postgres is true): one child row
// pointing at a parent that is deleted, and one control row pointing at a
// parent that survives. After each delete path:
//   - CASCADE edge: no child row references the deleted parent;
//   - SET NULL edge: the child row survives with the column NULL;
//   - control rows: untouched.
//
// Postgres does this natively (ON DELETE CASCADE / SET NULL); SQLite runs with
// foreign_keys(OFF), so before GitHub #361 (2026-10-13) every child of a
// deleted task, execution or artifact outlived it there.
//
// see LLD § https://docs.vornik.io §6
func RunCascadeDeleteSuite(t *testing.T, db *sql.DB, repos CascadeDeleteRepos, seedTime func(time.Time) any, postgres bool) {
	t.Helper()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	newFixture := func(t *testing.T) *cascadeFixture {
		return &cascadeFixture{t: t, db: db, repos: repos, seedTime: seedTime, postgres: postgres, now: now,
			project: uniqueID("casc-proj")}
	}

	t.Run("every graph edge has a seeder", func(t *testing.T) {
		for _, e := range persistence.SQLiteCascadeGraph {
			// Edges below knowledge_entities / project_memory_chunks are
			// exercised by the project wipe suite (GitHub #365), whose
			// seeder is generic; this suite roots only at these three.
			if !cascadeParentTables[e.Parent] {
				continue
			}
			if _, ok := cascadeChildSeeders[e.Child]; !ok && !cascadeParentTables[e.Child] {
				t.Errorf("edge %s.%s -> %s: no seeder in cascadeChildSeeders; add one", e.Child, e.Column, e.Parent)
			}
		}
	})

	t.Run("TaskRepository.Delete", func(t *testing.T) {
		f := newFixture(t)
		f.build("tasks")
		if err := repos.Tasks.Delete(context.Background(), f.victim["tasks"]); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		f.verify()
	})

	t.Run("ArtifactRepository.Delete", func(t *testing.T) {
		f := newFixture(t)
		f.build("artifacts")
		if err := repos.Artifacts.Delete(context.Background(), f.victim["artifacts"]); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		f.verify()
	})

	t.Run("ArtifactRepository.DeleteByExecutionID", func(t *testing.T) {
		f := newFixture(t)
		f.build("artifacts")
		// The victim artifact hangs off the survivor's sibling execution
		// victimExec; the control artifact off the survivor execution.
		if err := repos.Artifacts.DeleteByExecutionID(context.Background(), f.artifactExec); err != nil {
			t.Fatalf("DeleteByExecutionID: %v", err)
		}
		f.verify()
	})

	for _, root := range []string{"tasks", "executions", "artifacts"} {
		t.Run("retention sweep: "+root+" step", func(t *testing.T) {
			newFixture(t).sweepRoot(root)
		})
	}
}

// sweepRoot ages the root victim, runs the per-project sweep and checks that
// the step under test removed exactly the victim, then verifies the children.
func (f *cascadeFixture) sweepRoot(root string) {
	t := f.t
	f.build(root)
	f.age(root)
	sw := retention.New(f.db, zerolog.Nop(), retention.WithClock(func() time.Time { return f.now }))
	// Every other window far out: only the three steps under test select
	// anything, so a child can only go through the cascade.
	const far = 3650
	c, err := sw.Sweep(context.Background(), retention.Policy{ProjectID: f.project,
		TasksDays: 60, ExecutionsDays: 60, ArtifactsDays: 60,
		TaskLLMUsageDays: far, ToolAuditDays: far, TaskMessagesDays: far, MemoryIngestAuditDays: far,
		MemoryPolicyEvalAllowDays: far, MemoryPolicyEvalBlockDays: far, MemoryEvictionAuditDays: far,
		ChatAuditDays: far})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	got := map[string]int{"tasks": c.Tasks, "executions": c.Executions, "artifacts": c.Artifacts}[root]
	if got != 1 {
		t.Errorf("Counts for %s = %d, want 1 (the victim only)", root, got)
	}
	f.verify()
}

// cascadeParentTables are created through the repositories, not seeders.
var cascadeParentTables = map[string]bool{"tasks": true, "executions": true, "artifacts": true}

// cascadeRow is the reference columns of one seeded child row; every
// reference the edge under test does not set points at a survivor.
type cascadeRow struct {
	id, project, task, exec, artifact string
	n                                 int // unique small integer (chat/thread ids, seq)
}

// cascadeChildSeeders insert one row of a child table on either driver.
// Columns: the union of both drivers' NOT NULL columns without a default.
var cascadeChildSeeders = map[string]func(f *cascadeFixture, r cascadeRow) error{
	"webhook_events": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO webhook_events (id, project_id, source, payload_hash, status, task_id, created_at)
			VALUES ($1, $2, 'test', $1, 'processed', $3, $4)`, r.id, r.project, r.task, f.ts())
	},
	"autonomy_evaluations": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO autonomy_evaluations (id, project_id, outcome, task_id, created_at)
			VALUES ($1, $2, 'enqueued', $3, $4)`, r.id, r.project, r.task, f.ts())
	},
	"trading_orders": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO trading_orders (id, project_id, idempotency_key, mode, symbol, action, order_type,
			qty, status, task_id, submitted_at)
			VALUES ($1, $2, $1, 'paper', 'AAPL', 'BUY', 'MKT', 1, 'SUBMITTED', $3, $4)`, r.id, r.project, r.task, f.ts())
	},
	"task_judge_verdicts": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO task_judge_verdicts (id, project_id, task_id, role, model, verdict, recorded_at)
			VALUES ($1, $2, $3, 'judge', 'm', 'pass', $4)`, r.id, r.project, r.task, f.ts())
	},
	"task_post_mortems": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO task_post_mortems (task_id, project_id, summary, model, recorded_at)
			VALUES ($1, $2, 's', 'm', $3)`, r.task, r.project, f.ts())
	},
	"task_messages": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO task_messages (id, task_id, execution_id, author_kind, message_kind, content, created_at)
			VALUES ($1, $2, $3, 'system', 'note', 'c', $4)`, r.id, r.task, r.exec, f.ts())
	},
	"task_scratchpad": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO task_scratchpad (task_id, last_execution_id) VALUES ($1, $2)`, r.task, r.exec)
	},
	"telegram_task_threads": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO telegram_task_threads (id, task_id, chat_id, thread_id, topic_name, created_at)
			VALUES ($1, $2, $3, $3, 't', $4)`, r.id, r.task, r.n, f.ts())
	},
	"task_credentials": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO task_credentials (id, task_id, execution_id, tool, label, value, created_at)
			VALUES ($1, $2, $3, 'git', 'l', 'v', $4)`, r.id, r.task, r.exec, f.ts())
	},
	"execution_narration": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO execution_narration (id, project_id, task_id, execution_id, seq, kind, text, created_at)
			VALUES ($1, $2, $3, $4, $5, 'step', 't', $6)`, r.id, r.project, r.task, r.exec, r.n, f.ts())
	},
	"execution_quality_scores": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO execution_quality_scores (execution_id, project_id, task_id, workflow_id,
			workflow_revision, scorer_version, status, recorded_at)
			VALUES ($1, $2, $3, 'wf', 'r1', 'v1', 'unscorable', $4)`, r.exec, r.project, r.task, f.ts())
	},
	"llm_exchanges": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO llm_exchanges (id, execution_id, step_id, seq, request_hash, request_json,
			response_json, recorded_at)
			VALUES ($1, $2, 's', $3, 'h', '{}', '{}', $4)`, r.id, r.exec, r.n%1_000_000_000, f.ts()) // seq is INTEGER on Postgres
	},
	"corpus_epochs": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO corpus_epochs (id, project_id, ingest_execution_id, created_at)
			VALUES ($1, $2, $3, $4)`, r.id, r.project, r.exec, f.ts())
	},
	"project_ingest_queue": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO project_ingest_queue (id, project_id, source_artifact_id, producer_role,
			ingest_execution_id, enqueued_at)
			VALUES ($1, $2, $3, 'r', $4, $5)`, r.id, r.project, r.artifact, r.exec, f.ts())
	},
	"project_memory_chunks": func(f *cascadeFixture, r cascadeRow) error {
		if !f.postgres { // SQLite's chunk table has artifact_id but no task_id / source_name
			return f.exec(`INSERT INTO project_memory_chunks (id, project_id, artifact_id, content, content_hash, created_at)
				VALUES ($1, $2, $3, 'c', $1, $4)`, r.id, r.project, r.artifact, f.ts())
		}
		return f.exec(`INSERT INTO project_memory_chunks (id, project_id, task_id, artifact_id, source_name, content,
			content_hash) VALUES ($1, $2, $3, $4, 's', 'c', $1)`, r.id, r.project, r.task, r.artifact)
	},
	"project_memory_quarantine": func(f *cascadeFixture, r cascadeRow) error {
		return f.exec(`INSERT INTO project_memory_quarantine (id, project_id, source_artifact_id, ingest_execution_id,
			content, content_hash, failed_gate)
			VALUES ($1, $2, $3, $4, 'c', $1, 'g')`, r.id, r.project, r.artifact, r.exec)
	},
}

type cascadeFixture struct {
	t        *testing.T
	db       *sql.DB
	repos    CascadeDeleteRepos
	seedTime func(time.Time) any
	postgres bool
	now      time.Time
	project  string

	// victim is the id of the row of each parent table the delete removes
	// (only the tables below the root); survivor of the row that stays.
	victim, survivor map[string]string
	// artifactExec is the execution the victim artifact hangs off when the
	// root is artifacts (DeleteByExecutionID deletes by it).
	artifactExec string
	checks       []func()
}

// cascadeSeq numbers rows whose unique keys are integers (telegram chat and
// thread ids, seq). Seeded from the clock so reruns on a shared integration
// database do not collide.
var cascadeSeq = func() *atomic.Int64 {
	var a atomic.Int64
	a.Store(time.Now().UnixNano() / 1000 % 1_000_000_000_000)
	return &a
}()

func (f *cascadeFixture) ts() any { return f.seedTime(f.now) }

func (f *cascadeFixture) exec(q string, args ...any) error {
	_, err := f.db.ExecContext(context.Background(), q, args...)
	return err
}

func (f *cascadeFixture) id(prefix string) string { return uniqueID("casc-" + prefix) }

// newTask creates a terminal task stamped a day before the suite clock.
func (f *cascadeFixture) newTask(parent *string) string {
	return f.newTaskAt(parent, f.now.AddDate(0, 0, -1))
}

// newTaskAt stamps created_at/updated_at at creation: Postgres's
// update_tasks_updated_at trigger overwrites updated_at on every UPDATE, so a
// task cannot be aged afterwards.
func (f *cascadeFixture) newTaskAt(parent *string, at time.Time) string {
	f.t.Helper()
	id := f.id("task")
	wf := "wf"
	if err := f.repos.Tasks.Create(context.Background(), &persistence.Task{ID: id, ProjectID: f.project,
		WorkflowID: &wf, Status: persistence.TaskStatusCompleted, ParentTaskID: parent, Payload: []byte(`{}`),
		CreatedAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatalf("create task: %v", err)
	}
	return id
}

func (f *cascadeFixture) newExec(task string) string {
	f.t.Helper()
	id := f.id("exec")
	if err := f.repos.Executions.Create(context.Background(), &persistence.Execution{ID: id, TaskID: task,
		ProjectID: f.project, WorkflowID: "wf", WorkflowRevision: "r1",
		Status: persistence.ExecutionStatusRunning}); err != nil {
		f.t.Fatalf("create execution: %v", err)
	}
	return id
}

func (f *cascadeFixture) newArtifact(task, exec string) string {
	f.t.Helper()
	id := f.id("art")
	if err := f.repos.Artifacts.Create(context.Background(), &persistence.Artifact{ID: id, ProjectID: f.project,
		TaskID: &task, ExecutionID: &exec, Name: "a.txt", ArtifactClass: persistence.ArtifactClassOutput,
		StoragePath: "/nonexistent/" + id, CreatedAt: f.now}); err != nil {
		f.t.Fatalf("create artifact: %v", err)
	}
	return id
}

// build creates the parent rows for a delete rooted at root, then seeds every
// edge whose parent is root or below it.
//
//	root tasks:      victim T ← exec ET ← artifact AT; survivor U ← EU ← AU
//	root executions: victim exec EV (task U) ← AV;      survivor U ← EU ← AU
//	root artifacts:  victim AV (exec EV2 of U);         survivor U ← EU ← AU
func (f *cascadeFixture) build(root string) {
	f.t.Helper()
	u := f.newTask(nil)
	eu := f.newExec(u)
	au := f.newArtifact(u, eu)
	f.survivor = map[string]string{"tasks": u, "executions": eu, "artifacts": au}
	f.victim = map[string]string{}
	switch root {
	case "tasks":
		tv := f.newTaskAt(nil, f.now.AddDate(0, 0, -100)) // due under the 60-day tasks window
		ev := f.newExec(tv)
		f.victim = map[string]string{"tasks": tv, "executions": ev, "artifacts": f.newArtifact(tv, ev)}
	case "executions":
		ev := f.newExec(u)
		f.victim = map[string]string{"executions": ev, "artifacts": f.newArtifact(u, ev)}
	case "artifacts":
		f.artifactExec = f.newExec(u)
		f.victim = map[string]string{"artifacts": f.newArtifact(u, f.artifactExec)}
	}
	// The parent rows themselves.
	for table, id := range f.victim {
		table, id := table, id
		f.checks = append(f.checks, func() {
			if n := f.count(table, "id", id); n != 0 {
				f.t.Errorf("%s %s: still present after the delete", table, id)
			}
		})
	}
	for table, id := range f.survivor {
		table, id := table, id
		f.checks = append(f.checks, func() {
			if n := f.count(table, "id", id); n != 1 {
				f.t.Errorf("survivor %s %s: present=%d, want 1", table, id, n)
			}
		})
	}
	for _, e := range persistence.SQLiteCascadeGraph {
		if e.PostgresOnly && !f.postgres {
			continue
		}
		if _, ok := f.victim[e.Parent]; !ok {
			continue
		}
		f.seedEdge(e)
	}
}

// seedEdge seeds the victim row and the control row of one edge.
func (f *cascadeFixture) seedEdge(e persistence.FKEdge) {
	f.t.Helper()
	victimID := f.seedChild(e, f.victim[e.Parent])
	controlID := f.seedChild(e, f.survivor[e.Parent])
	label := e.Child + "." + e.Column + " (" + e.Action.String() + ")"
	key := "id"
	if e.Child == "task_post_mortems" || e.Child == "task_scratchpad" {
		key = "task_id"
	}
	if e.Child == "execution_quality_scores" {
		key = "execution_id"
	}
	f.checks = append(f.checks, func() {
		switch e.Action {
		case persistence.FKCascade:
			if n := f.count(e.Child, e.Column, f.victim[e.Parent]); n != 0 {
				f.t.Errorf("%s: %d row(s) still reference deleted %s %s", label, n, e.Parent, f.victim[e.Parent])
			}
		case persistence.FKSetNull:
			got, present := f.column(e.Child, key, victimID, e.Column)
			if !present {
				f.t.Errorf("%s: row %s was deleted; SET NULL must keep it", label, victimID)
			} else if got.Valid {
				f.t.Errorf("%s: row %s still references %q, want NULL", label, victimID, got.String)
			}
		}
		got, present := f.column(e.Child, key, controlID, e.Column)
		if !present || got.String != f.survivor[e.Parent] {
			f.t.Errorf("%s: control row %s changed (present=%v, %s=%q, want %q)",
				label, controlID, present, e.Column, got.String, f.survivor[e.Parent])
		}
	})
}

// seedChild inserts one row of e.Child whose e.Column is parentID and whose
// other references point at survivors, and returns the row's key.
func (f *cascadeFixture) seedChild(e persistence.FKEdge, parentID string) string {
	f.t.Helper()
	switch e.Child {
	case "tasks":
		return f.newTask(&parentID)
	case "executions":
		return f.newExec(parentID)
	case "artifacts":
		if e.Column == "task_id" {
			return f.newArtifact(parentID, f.survivor["executions"])
		}
		return f.newArtifact(f.survivor["tasks"], parentID)
	}
	r := cascadeRow{id: f.id(e.Child), project: f.id("p"), task: f.survivor["tasks"],
		exec: f.survivor["executions"], artifact: f.survivor["artifacts"], n: int(cascadeSeq.Add(1))}
	switch e.Column {
	case "task_id":
		r.task = parentID
	case "execution_id", "ingest_execution_id", "last_execution_id":
		r.exec = parentID
	case "source_artifact_id", "artifact_id":
		r.artifact = parentID
	default:
		f.t.Fatalf("seedChild: no mapping for column %s.%s", e.Child, e.Column)
	}
	// task_scratchpad's key is task_id: a row for its execution edge needs a
	// task of its own, or the victim and control rows collide.
	if e.Child == "task_scratchpad" && e.Column != "task_id" {
		r.task = f.newTask(nil)
	}
	seed := cascadeChildSeeders[e.Child]
	if seed == nil {
		f.t.Fatalf("seedChild: no seeder for %s", e.Child)
	}
	if err := seed(f, r); err != nil {
		f.t.Fatalf("seed %s: %v", e.Child, err)
	}
	switch e.Child {
	case "task_post_mortems", "task_scratchpad":
		return r.task
	case "execution_quality_scores":
		return r.exec
	}
	return r.id
}

// age makes the root victim due under a 60-day policy; everything else stays
// young, so only the cascade can remove a child.
func (f *cascadeFixture) age(root string) {
	f.t.Helper()
	old := f.seedTime(f.now.AddDate(0, 0, -100))
	young := f.seedTime(f.now.AddDate(0, 0, -1))
	ctx := context.Background()
	must := func(q string, args ...any) {
		if _, err := f.db.ExecContext(ctx, q, args...); err != nil {
			f.t.Fatalf("age: %v\n%s", err, q)
		}
	}
	// Tasks are stamped at creation (newTaskAt): all terminal and a day old
	// except the tasks-root victim, created 100 days old. Every execution and
	// artifact young (executions terminal) first, so the sweep's own
	// predicates select only the victim.
	must(`UPDATE executions SET status = 'COMPLETED', created_at = $1 WHERE project_id = $2`, young, f.project)
	must(`UPDATE artifacts SET created_at = $1 WHERE project_id = $2`, young, f.project)
	switch root {
	case "executions":
		must(`UPDATE executions SET created_at = $1 WHERE id = $2`, old, f.victim["executions"])
	case "artifacts":
		must(`UPDATE artifacts SET created_at = $1 WHERE id = $2`, old, f.victim["artifacts"])
	}
}

func (f *cascadeFixture) verify() {
	f.t.Helper()
	for _, c := range f.checks {
		c()
	}
}

func (f *cascadeFixture) count(table, col, val string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`, table, col), val).Scan(&n); err != nil {
		f.t.Fatalf("count %s.%s: %v", table, col, err)
	}
	return n
}

func (f *cascadeFixture) column(table, key, keyVal, col string) (sql.NullString, bool) {
	f.t.Helper()
	var v sql.NullString
	err := f.db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1`, col, table, key), keyVal).Scan(&v)
	if err == sql.ErrNoRows {
		return v, false
	}
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		f.t.Fatalf("read %s.%s: %v", table, col, err)
	}
	return v, err == nil
}

package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Found-at-level regression for GitHub #410 (found at code review,
// 2026-10-16, while fixing #408; cli-design.md records it). Cancelling a
// parent task cascaded only to PENDING, QUEUED, LEASED, RUNNING and
// WAITING_FOR_CHILDREN descendants: a PAUSED, AWAITING_INPUT,
// AWAITING_EXTERNAL or AWAITING_APPROVAL descendant survived, unfinished,
// under a CANCELLED parent. A user observes it after `vornikctl task cancel
// <parent>`, so this drives that command (runTaskCancel) against the real
// API handler over HTTP, a real executor as the cancel teardown (its
// CancelChildren is the cascade every cancel entry point reaches), and real
// migrated repositories. The scenario runs on SQLite here and on Postgres in
// the integration lane (task_cancel_410_integration_test.go), 05-scheduler
// §4.11 "Amended 2026-10-17".

// run410 seeds the tree, cancels the parent with the operator's command and
// asserts no unfinished descendant remains:
//
//	parent   WAITING_FOR_CHILDREN   (the cancel target)
//	├─ paused     PAUSED            (with a PAUSED execution row)
//	│  └─ approval  AWAITING_APPROVAL
//	├─ external   AWAITING_EXTERNAL (expected_by set: the monitor would re-queue it)
//	└─ done       COMPLETED         (finished: left as it is)
func run410(t *testing.T, tasks persistence.TaskRepository, execs persistence.ExecutionRepository, project string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	ids := map[string]string{}
	mk := func(name string, st persistence.TaskStatus, parent string, expectedBy *time.Time) {
		t.Helper()
		id := project + "-" + name
		ids[name] = id
		wf := "wf"
		var pp *string
		if parent != "" {
			p := ids[parent]
			pp = &p
		}
		if err := tasks.Create(ctx, &persistence.Task{
			ID: id, ProjectID: project, WorkflowID: &wf, Status: st, ParentTaskID: pp,
			CreationSource: persistence.TaskCreationSourceUser, Priority: 50,
			Payload: []byte(`{}`), CreatedAt: at, UpdatedAt: at, MaxAttempts: 3, Attempt: 1,
			ExpectedBy: expectedBy,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	mk("parent", persistence.TaskStatusWaitingForChildren, "", nil)
	mk("paused", persistence.TaskStatusPaused, "parent", nil)
	mk("approval", persistence.TaskStatusAwaitingApproval, "paused", nil)
	mk("external", persistence.TaskStatusAwaitingExternal, "parent", &future)
	mk("done", persistence.TaskStatusCompleted, "parent", nil)

	pausedExec := project + "-exec-paused"
	if err := execs.Create(ctx, &persistence.Execution{
		ID: pausedExec, TaskID: ids["paused"], ProjectID: project, WorkflowID: "wf",
		WorkflowRevision: "v1", Status: persistence.ExecutionStatusPaused,
		CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("create paused execution: %v", err)
	}

	exec := executor.NewWithOptions(nil, execs, nil, tasks, executor.DefaultConfig())
	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithTaskRepository(tasks),
		api.WithExecutionRepository(execs),
		api.WithExecutor(exec),
	).Routes())
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key-410")

	taskCov_reset()
	taskCancelProject = project
	out, err := captureStdoutFunc(t, func() error { return runTaskCancel(taskCancelCmd, []string{ids["parent"]}) })
	if err != nil {
		t.Fatalf("vornikctl task cancel %s: %v\n%s", ids["parent"], err, out)
	}

	unfinished := persistence.CancellableTaskStatuses()
	get := func(name string) *persistence.Task {
		t.Helper()
		got, gerr := tasks.Get(ctx, ids[name])
		if gerr != nil || got == nil {
			t.Fatalf("get %s: %v", name, gerr)
		}
		return got
	}
	for _, name := range []string{"parent", "paused", "approval", "external"} {
		if got := get(name); slices.Contains(unfinished, got.Status) || got.Status != persistence.TaskStatusCancelled {
			t.Errorf("%s = %s after the parent's cancel, want CANCELLED (no unfinished descendant may remain)", name, got.Status)
		}
	}
	for _, name := range []string{"paused", "approval", "external"} {
		got := get(name)
		if got.LastError == nil || !strings.Contains(*got.LastError, ids["parent"]) {
			t.Errorf("%s: last_error = %v, want the cascade's reason naming %s", name, got.LastError, ids["parent"])
		}
	}
	if got := get("done"); got.Status != persistence.TaskStatusCompleted {
		t.Errorf("done = %s, want COMPLETED (a finished child is left as it is)", got.Status)
	}
	// The whole tree, walked the way the cascade walks it: nothing unfinished.
	var walk func(id string)
	walk = func(id string) {
		children, cerr := tasks.GetChildren(ctx, id)
		if cerr != nil {
			t.Fatalf("children of %s: %v", id, cerr)
		}
		for _, c := range children {
			if slices.Contains(unfinished, c.Status) {
				t.Errorf("descendant %s is still %s", c.ID, c.Status)
			}
			walk(c.ID)
		}
	}
	walk(ids["parent"])

	if e, gerr := execs.Get(ctx, pausedExec); gerr != nil || e == nil {
		t.Fatalf("get paused execution: %v", gerr)
	} else if e.Status == persistence.ExecutionStatusPaused {
		t.Errorf("the cancelled PAUSED child's execution row is still PAUSED, want it superseded")
	}
}

func TestTaskCancel410_ParentCancelEndsEveryDescendant(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	run410(t, sqlite.NewTaskRepository(db.DB), sqlite.NewExecutionRepository(db.DB), "p410")
}

// TestTaskRetry410_SupersededAttemptsParkedChildrenEnd — GitHub #410, the
// other CancelChildren caller (code review review-20261017-2040 F1): `vornikctl
// task retry` on a FAILED parent cancels the superseded attempt's children
// before detaching them. Before #410 a PAUSED or AWAITING_EXTERNAL child was
// detached alive, an unfinished task with no parent.
func TestTaskRetry410_SupersededAttemptsParkedChildrenEnd(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	ctx := context.Background()
	const project = "r410"
	at := time.Now().UTC().Add(-time.Hour)
	wf := "wf"
	parent := "r410-parent"
	for id, st := range map[string]persistence.TaskStatus{
		parent:          persistence.TaskStatusFailed,
		"r410-paused":   persistence.TaskStatusPaused,
		"r410-external": persistence.TaskStatusAwaitingExternal,
	} {
		var pp *string
		if id != parent {
			p := parent
			pp = &p
		}
		if err := tasks.Create(ctx, &persistence.Task{
			ID: id, ProjectID: project, WorkflowID: &wf, Status: st, ParentTaskID: pp,
			CreationSource: persistence.TaskCreationSourceUser, Priority: 50,
			Payload: []byte(`{}`), CreatedAt: at, UpdatedAt: at, MaxAttempts: 3, Attempt: 1,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	exec := executor.NewWithOptions(nil, execs, nil, tasks, executor.DefaultConfig())
	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithTaskRepository(tasks),
		api.WithExecutionRepository(execs),
		api.WithExecutor(exec),
	).Routes())
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key-410")

	taskCov_reset()
	taskRetryProject = project
	out, err := captureStdoutFunc(t, func() error { return runTaskRetry(taskRetryCmd, []string{parent}) })
	if err != nil {
		t.Fatalf("vornikctl task retry %s: %v\n%s", parent, err, out)
	}
	for _, id := range []string{"r410-paused", "r410-external"} {
		got, gerr := tasks.Get(ctx, id)
		if gerr != nil || got == nil {
			t.Fatalf("get %s: %v", id, gerr)
		}
		if got.Status != persistence.TaskStatusCancelled {
			t.Errorf("%s = %s after the parent's retry, want CANCELLED (a superseded attempt's child must not be detached alive)", id, got.Status)
		}
	}
}

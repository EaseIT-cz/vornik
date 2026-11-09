package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
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

// Regression for GitHub #490 (05-scheduler §4.11 "Amended 2026-11-07"). Found
// at review, observed at production: a parent's cancel closed a child's
// pending approval with the generic cascade reason, and the approval vanished
// from the inbox with no word of which task above it was cancelled. Driven
// the way #410's scenario is: `vornikctl task cancel` over the real API
// handler, a real executor, real migrated repositories; SQLite here, Postgres
// in task_cancel_490_integration_test.go.
func run490(t *testing.T, tasks persistence.TaskRepository, execs persistence.ExecutionRepository, project string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	ids := map[string]string{}
	mk := func(name string, st persistence.TaskStatus, parent string, updated time.Time, lastErr *string) {
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
			Payload: []byte(`{}`), CreatedAt: updated, UpdatedAt: updated, MaxAttempts: 3, Attempt: 1,
			LastError: lastErr,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("parent", persistence.TaskStatusWaitingForChildren, "", at, nil)
	mk("approval", persistence.TaskStatusAwaitingApproval, "parent", at, nil)
	mk("input", persistence.TaskStatusAwaitingInput, "parent", at, nil)
	old := persistence.ApprovalClosedPrefix + "task x above it was cancelled"
	mk("old", persistence.TaskStatusCancelled, "", time.Now().UTC().Add(-48*time.Hour), &old)

	exec := executor.NewWithOptions(nil, execs, nil, tasks, executor.DefaultConfig())
	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithTaskRepository(tasks),
		api.WithExecutionRepository(execs),
		api.WithExecutor(exec),
	).Routes())
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key-490")

	taskCov_reset()
	taskCancelProject = project
	out, err := captureStdoutFunc(t, func() error { return runTaskCancel(taskCancelCmd, []string{ids["parent"]}) })
	if err != nil {
		t.Fatalf("vornikctl task cancel %s: %v\n%s", ids["parent"], err, out)
	}

	lastErr := func(name string) string {
		t.Helper()
		got, gerr := tasks.Get(ctx, ids[name])
		if gerr != nil || got == nil {
			t.Fatalf("get %s: %v", name, gerr)
		}
		if got.Status != persistence.TaskStatusCancelled || got.LastError == nil {
			t.Fatalf("%s = %s, last_error %v; want CANCELLED with a reason", name, got.Status, got.LastError)
		}
		return *got.LastError
	}
	want := persistence.ApprovalClosedPrefix + "task " + ids["parent"] + " above it was cancelled"
	if got := lastErr("approval"); got != want {
		t.Errorf("approval child: last_error %q, want %q", got, want)
	}
	if got := lastErr("input"); strings.HasPrefix(got, persistence.ApprovalClosedPrefix) || !strings.Contains(got, ids["parent"]) {
		t.Errorf("input child: last_error %q, want the generic cascade reason", got)
	}

	closed, err := tasks.ListApprovalsClosedSince(ctx, []string{project}, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListApprovalsClosedSince: %v", err)
	}
	if len(closed) != 1 || closed[0].ID != ids["approval"] {
		got := make([]string, 0, len(closed))
		for _, c := range closed {
			got = append(got, c.ID)
		}
		t.Fatalf("ListApprovalsClosedSince = %v, want only %s (the 48-hour-old row is outside the window)", got, ids["approval"])
	}
	if other, _ := tasks.ListApprovalsClosedSince(ctx, []string{project + "-other"}, time.Now().UTC().Add(-24*time.Hour)); len(other) != 0 {
		t.Fatalf("another project's scope listed %d rows", len(other))
	}
}

func TestTaskCancel490_ClosedApprovalNamesItsParent(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	run490(t, sqlite.NewTaskRepository(db.DB), sqlite.NewExecutionRepository(db.DB), "p490")
}

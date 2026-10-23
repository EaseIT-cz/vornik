package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/taskcancel"
)

// Found-at-level regression for GitHub #421 (found at review, 2026-10-17,
// by lane W13-E while fixing #410; 05-scheduler §4.8 "Amended 2026-10-17
// (GitHub #421)"). Executor.Resume read the task without checking its
// status and then wrote it RUNNING with a plain UpdateStatus, so a cancel
// that landed before that write was overwritten and the run started. A user
// observes it through the resume request, so this drives REST
// POST /api/v1/executions/{id}/resume over HTTP against the real API
// handler, a real executor and real migrated repositories. The scenario runs
// on SQLite here and on Postgres in the integration lane
// (resume_421_integration_test.go).

// cancelOnResumeRead is the seam: the executor's execution repository, whose
// GetByTaskID (the read Resume makes after reading the task and before any
// write) returns the PAUSED row and then cancels the task through the real
// cancel primitive, as a cancel whose teardown found no live execution.
type cancelOnResumeRead struct {
	persistence.ExecutionRepository
	tasks  persistence.TaskRepository
	taskID string
	once   sync.Once
	t      *testing.T
}

func (r *cancelOnResumeRead) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	e, err := r.ExecutionRepository.GetByTaskID(ctx, taskID)
	if taskID == r.taskID {
		r.once.Do(func() {
			out, cerr := taskcancel.Cancel(context.Background(), r.tasks, nil, taskID,
				persistence.ClientCancellableTaskStatuses())
			if cerr != nil || !out.Transitioned {
				r.t.Errorf("seam: cancel of %s did not transition (err=%v)", taskID, cerr)
			}
		})
	}
	return e, err
}

// run421 seeds a PAUSED task with a PAUSED execution and asks the API to
// resume it while (raced=true) or after (raced=false) a cancel lands.
func run421(t *testing.T, tasks persistence.TaskRepository, execs persistence.ExecutionRepository, project string, raced bool) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	wf := "wf"
	taskID := project + "-task"
	execID := project + "-exec"
	if err := tasks.Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: project, WorkflowID: &wf, Status: persistence.TaskStatusPaused,
		CreationSource: persistence.TaskCreationSourceUser, Priority: 50,
		Payload: []byte(`{}`), CreatedAt: at, UpdatedAt: at, MaxAttempts: 3, Attempt: 1,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := execs.Create(ctx, &persistence.Execution{
		ID: execID, TaskID: taskID, ProjectID: project, WorkflowID: "wf",
		WorkflowRevision: "v1", Status: persistence.ExecutionStatusPaused,
		CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}

	execRepo := execs
	if raced {
		execRepo = &cancelOnResumeRead{ExecutionRepository: execs, tasks: tasks, taskID: taskID, t: t}
	} else {
		if out, err := taskcancel.Cancel(ctx, tasks, nil, taskID, persistence.ClientCancellableTaskStatuses()); err != nil || !out.Transitioned {
			t.Fatalf("cancel before resume did not transition (err=%v)", err)
		}
	}
	exec := executor.NewWithOptions(nil, execRepo, nil, tasks, executor.DefaultConfig())
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.Stop(sctx)
	})
	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithTaskRepository(tasks),
		api.WithExecutionRepository(execs),
		api.WithExecutor(exec),
	).Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/executions/"+execID+"/resume", "application/json", nil)
	if err != nil {
		t.Fatalf("POST resume: %v", err)
	}
	_ = resp.Body.Close()

	if exec.IsExecuting(taskID) {
		t.Errorf("a run was started for a cancelled task")
	}
	got, err := tasks.Get(ctx, taskID)
	if err != nil || got == nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != persistence.TaskStatusCancelled {
		t.Errorf("task = %s after a resume raced its cancel, want CANCELLED (the cancel must not be overwritten)", got.Status)
	}
	e, err := execs.Get(ctx, execID)
	if err != nil || e == nil {
		t.Fatalf("get execution: %v", err)
	}
	if e.Status == persistence.ExecutionStatusRunning {
		t.Errorf("execution = RUNNING for a cancelled task, want it left as it was")
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("resume answered %d, want 409 (refused: the task is no longer resumable)", resp.StatusCode)
	}
}

func TestResume421_CancelBetweenReadAndWriteWins(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	run421(t, sqlite.NewTaskRepository(db.DB), sqlite.NewExecutionRepository(db.DB), "r421", true)
}

func TestResume421_CancelledBeforeResumeStaysCancelled(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	run421(t, sqlite.NewTaskRepository(db.DB), sqlite.NewExecutionRepository(db.DB), "c421", false)
}

// The seam delegates its lookup, so it keeps the real repository's miss
// contract (repo-double-conformance).
func TestResume421_SeamKeepsTheMissContract(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	seam := &cancelOnResumeRead{ExecutionRepository: sqlite.NewExecutionRepository(db.DB),
		tasks: sqlite.NewTaskRepository(db.DB), taskID: "never-looked-up", t: t}
	repotest.AssertMissRepo(t, "ExecutionRepository.GetByTaskID", seam.GetByTaskID)
}

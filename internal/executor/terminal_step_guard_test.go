package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/taskcancel"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running.
// task_20261008201257_966e43b86a7aad23 was cancelled during its `curate`
// step by a path that only flipped the row. The curator ran on, the executor
// dispatched `recommend` and started a second container, and handleSuccess
// wrote COMPLETED over CANCELLED. 05-scheduler.md §4.11.

// twoStepExecutor runs a real executor over curate → recommend → done, with
// flip applied to the task row while the FIRST container starts.
func twoStepExecutor(t *testing.T, taskID string, flip persistence.TaskStatus) (*Executor, *MockRuntime, *MockTaskRepo, *MockExecRepo) {
	t.Helper()
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "curator", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
				{Name: "recommender", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "curate",
				Steps: map[string]registry.WorkflowStep{
					"curate":    {Type: "agent", Role: "curator", OnSuccess: "recommend"},
					"recommend": {Type: "agent", Role: "recommender", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"find venues"}}`),
		CreatedAt: time.Now(),
	})
	var once sync.Once
	rt.onStart = func(*runtime.ContainerConfig) {
		// A row-only write, the shape of the pre-fix companion cancel: no
		// teardown, no context cancel. Only the step guard can stop step 2.
		once.Do(func() { _ = tr.UpdateStatus(context.Background(), taskID, flip) })
	}
	return e, rt, tr, er
}

func waitIdle(t *testing.T, e *Executor, taskID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.IsExecuting(taskID) {
		if time.Now().After(deadline) {
			t.Fatal("execution never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *MockRuntime) starts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCalls
}

func TestStepGuard_CancelledRowStopsNextStep(t *testing.T) {
	const taskID = "t-300-cancel"
	e, rt, tr, er := twoStepExecutor(t, taskID, persistence.TaskStatusCancelled)

	require.NoError(t, e.Execute(taskID))
	waitIdle(t, e, taskID)

	assert.Equal(t, 1, rt.starts(), "no container may start for a step after the task was cancelled")
	got, err := tr.Get(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status,
		"the finaliser must not write COMPLETED over CANCELLED")
	exec, err := er.GetByTaskID(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusCancelled, exec.Status)
}

// Another writer's terminal status (operator CLOSE) also stops the run, and
// the executor leaves that row as the other writer set it.
func TestStepGuard_ClosedRowStopsNextStepAndIsLeftAlone(t *testing.T) {
	const taskID = "t-300-closed"
	e, rt, tr, er := twoStepExecutor(t, taskID, persistence.TaskStatusClosed)

	require.NoError(t, e.Execute(taskID))
	waitIdle(t, e, taskID)

	assert.Equal(t, 1, rt.starts(), "no container may start for a step after the task was closed")
	got, err := tr.Get(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusClosed, got.Status, "the executor must not rewrite another writer's terminal row")
	exec, err := er.GetByTaskID(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusCancelled, exec.Status)
}

// The guard is the second line; a failed read must not stop healthy work.
func TestStepGuard_ReadErrorProceeds(t *testing.T) {
	const taskID = "t-300-readerr"
	e, _, _, _ := twoStepExecutor(t, taskID, persistence.TaskStatusRunning)
	task := &persistence.Task{ID: taskID, ProjectID: "p1"}
	stop, _ := e.taskTerminalBeforeStep(context.Background(), task, &persistence.Execution{ID: "x"}, "curate")
	assert.False(t, stop, "a live row must not stop the run")

	e.taskRepo = &failingGetRepo{TaskRepository: e.taskRepo}
	stop, _ = e.taskTerminalBeforeStep(context.Background(), task, &persistence.Execution{ID: "x"}, "curate")
	assert.False(t, stop, "a read error fails open")
}

// The guard stops on terminal statuses only. Every status a legitimate run
// can read inside the loop — RUNNING/LEASED during a run or a retry, and the
// parked statuses a resumed run passes through on its way back to RUNNING —
// must let the step proceed (05-scheduler.md §4.11, review c4af F7).
func TestStepGuard_NonTerminalStatusesProceed(t *testing.T) {
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusRunning, persistence.TaskStatusLeased, persistence.TaskStatusQueued,
		persistence.TaskStatusPending, persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput, persistence.TaskStatusAwaitingExternal,
		persistence.TaskStatusAwaitingApproval, persistence.TaskStatusPaused,
	} {
		tr := NewMockTaskRepo()
		tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: st})
		e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
		stop, err := e.taskTerminalBeforeStep(context.Background(), &persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "s")
		assert.False(t, stop, "status %s must not stop the run", st)
		assert.NoError(t, err)
	}
}

type failingGetRepo struct{ TaskRepository }

func (failingGetRepo) Get(context.Context, string) (*persistence.Task, error) {
	return nil, assert.AnError
}

// frozenGetRepo answers Get with a snapshot frozen BEFORE another writer
// moved the row, while every write acts on the live row underneath — the
// §4.10 F5 double. A finaliser that only re-reads and then writes plainly
// sees RUNNING here and overwrites; only a conditional write is refused.
type frozenGetRepo struct {
	*MockTaskRepo
	frozen persistence.Task
}

func (f *frozenGetRepo) Get(context.Context, string) (*persistence.Task, error) {
	cp := f.frozen
	return &cp, nil
}

// The finaliser window: the cancel (or close) lands after the last step's
// guard and before handleSuccess writes. The incident's last write was
// exactly this one — COMPLETED over CANCELLED.
func TestHandleSuccess_TerminalRowIsNotOverwritten(t *testing.T) {
	for _, live := range []persistence.TaskStatus{persistence.TaskStatusCancelled, persistence.TaskStatusClosed} {
		t.Run(string(live), func(t *testing.T) {
			rt := NewMockRuntime()
			er := NewMockExecRepo()
			tr := NewMockTaskRepo()
			tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: live})
			repo := &frozenGetRepo{MockTaskRepo: tr, frozen: persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning}}
			e := NewWithOptions(rt, er, NewMockArtifactRepo(), repo, nil)
			exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
			require.NoError(t, er.Create(context.Background(), exec))

			e.handleSuccess(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning}, exec, "", []byte(`{}`))

			got, _ := tr.Get(context.Background(), "t")
			assert.Equal(t, live, got.Status, "handleSuccess must not write COMPLETED over a row another writer finished")
		})
	}
}

// With an honest re-read, a cancelled row is finalised as a cancel.
func TestHandleSuccess_CancelledRowFinalisesAsCancelled(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusCancelled})
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleSuccess(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning}, exec, "", []byte(`{}`))

	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status)
	gotExec, _ := er.GetByTaskID(context.Background(), "t")
	assert.Equal(t, persistence.ExecutionStatusCancelled, gotExec.Status)
}

func TestHandleFailure_CancelledRowIsNotOverwritten(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusCancelled, MaxAttempts: 1, Attempt: 1})
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Attempt: 1}, exec, assert.AnError)

	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status)
}

// The shared primitive against a real executor that is running the task: the
// container is stopped (CancelIfActive is the executor's own live map).
func TestTaskCancel_RealExecutorStopsTheContainer(t *testing.T) {
	rt := &teardownRuntime{}
	e := liveExecutor(t, "task-1", "container-1", rt)

	out, err := taskcancel.Cancel(context.Background(), e.taskRepo, e, "task-1", persistence.CancellableTaskStatuses())

	require.NoError(t, err)
	assert.True(t, out.Transitioned)
	assert.True(t, out.TornDown, "the executor was running this task")
	assert.Equal(t, []string{"container-1"}, rt.stoppedIDs())
}

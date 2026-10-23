package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #421 (found at review, 2026-10-17): Executor.Resume wrote the task
// RUNNING with a plain UpdateStatus, so a resume racing a cancel overwrote
// CANCELLED and started the run. The found-at-level regression drives the
// REST resume (internal/cli/resume_421_test.go); these pin the executor's
// own refusals (05-scheduler §4.8, "Amended 2026-10-17 (GitHub #421)").

// A refused resume writes nothing: the execution stays PAUSED with its
// reason, no run starts.
func TestResume421_TerminalTaskRefusedWritesNothing(t *testing.T) {
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed,
		persistence.TaskStatusQueued, persistence.TaskStatusLeased,
	} {
		t.Run(string(st), func(t *testing.T) {
			e, _, er, _, tr := setup()
			tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: st, CreatedAt: time.Now()})
			exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusPaused}
			require.NoError(t, er.Create(context.Background(), exec))
			state := executionState{PausedReason: PauseReasonOperator}
			require.NoError(t, e.saveExecutionState(context.Background(), exec, state))

			_, err := e.Resume("t")
			require.ErrorIs(t, err, ErrResumeRefused)
			assert.Contains(t, err.Error(), string(st), "the refusal names the task's current status")
			assert.False(t, e.IsExecuting("t"), "no run may start")
			got, _ := tr.Get(context.Background(), "t")
			assert.Equal(t, st, got.Status)
			x, _ := er.Get(context.Background(), "x")
			assert.Equal(t, persistence.ExecutionStatusPaused, x.Status)
			assert.Equal(t, PauseReasonOperator, loadExecutionState(x).PausedReason, "the operator's reason is not cleared by a refused resume")
		})
	}
}

// supersedeOnRead returns the PAUSED execution row and then supersedes it,
// as the #410 cascade or the orphan-PAUSED backstop would between Resume's
// read and its execution write.
type supersedeOnRead struct {
	*MockExecRepo
	once sync.Once
}

func (s *supersedeOnRead) GetByTaskID(ctx context.Context, tid string) (*persistence.Execution, error) {
	x, err := s.MockExecRepo.GetByTaskID(ctx, tid)
	s.once.Do(func() { _ = s.UpdateStatus(ctx, x.ID, persistence.ExecutionStatusCancelled) })
	return x, err
}

func (s *supersedeOnRead) Get(ctx context.Context, id string) (*persistence.Execution, error) {
	x, err := s.MockExecRepo.Get(ctx, id)
	s.once.Do(func() { _ = s.UpdateStatus(ctx, id, persistence.ExecutionStatusCancelled) })
	return x, err
}

// The execution write is conditional too: a superseded execution is not
// reopened, and the task the gate moved is put back.
func TestResume421_SupersededExecutionRefusedAndTaskPutBack(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusPaused, CreatedAt: time.Now()})
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusPaused}))
	e.execRepo = &supersedeOnRead{MockExecRepo: er}

	_, err := e.Resume("t")
	require.ErrorIs(t, err, ErrResumeRefused)
	assert.False(t, e.IsExecuting("t"))
	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusPaused, got.Status, "the task the gate moved is put back")
	x, _ := er.Get(context.Background(), "x")
	assert.Equal(t, persistence.ExecutionStatusCancelled, x.Status, "a superseded execution is not reopened")
}

// ResumePaused (shutdown and retry-from-step pauses) uses the same
// conditional execution write.
func TestResumePaused421_SupersededExecutionNotReopened(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusPaused, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusPaused}
	require.NoError(t, er.Create(context.Background(), exec))
	require.NoError(t, e.saveExecutionState(context.Background(), exec, executionState{PausedReason: PauseReasonShutdown}))
	e.execRepo = &supersedeOnRead{MockExecRepo: er}

	err := e.ResumePaused("x")
	require.ErrorIs(t, err, ErrResumeRefused)
	assert.False(t, e.IsExecuting("t"))
	x, _ := er.Get(context.Background(), "x")
	assert.Equal(t, persistence.ExecutionStatusCancelled, x.Status)
}

// The CPC timeout scanner's caller wake: a cancel landing between its read
// (frozen at WAITING_FOR_CHILDREN) and its write is not overwritten with
// QUEUED.
func TestCPCWakeCaller421_CancelAfterReadIsNotOverwritten(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	tr.AddTask(&persistence.Task{ID: "caller", ProjectID: "p1", Status: persistence.TaskStatusCancelled})
	e.taskRepo = &frozenGetRepo{MockTaskRepo: tr, frozen: persistence.Task{ID: "caller", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren}}

	NewCPCTimeoutScanner(e).wakeCallerForTimeout(context.Background(), &persistence.CrossProjectCall{ID: "c", CallerTaskID: "caller"})

	got, _ := tr.Get(context.Background(), "caller")
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status, "a cancelled caller must not be re-queued")
}

// The caller wake still re-queues a caller that is waiting.
func TestCPCWakeCaller421_WaitingCallerIsRequeued(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	tr.AddTask(&persistence.Task{ID: "caller", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren})

	NewCPCTimeoutScanner(e).wakeCallerForTimeout(context.Background(), &persistence.CrossProjectCall{ID: "c", CallerTaskID: "caller"})

	got, _ := tr.Get(context.Background(), "caller")
	assert.Equal(t, persistence.TaskStatusQueued, got.Status)
}

// cancelOnExecRunning cancels the task row (a row-only cancel whose teardown
// found no handle) when runExecution marks the execution RUNNING, i.e. just
// before runExecution's own task write.
type cancelOnExecRunning struct {
	*MockExecRepo
	tr     *MockTaskRepo
	taskID string
	once   sync.Once
}

func (c *cancelOnExecRunning) UpdateStatus(ctx context.Context, id string, st persistence.ExecutionStatus) error {
	if st == persistence.ExecutionStatusRunning {
		c.once.Do(func() { _ = c.tr.UpdateStatus(ctx, c.taskID, persistence.TaskStatusCancelled) })
	}
	return c.MockExecRepo.UpdateStatus(ctx, id, st)
}

// runExecution's entry write no longer reopens a cancelled row, so the step
// guard sees CANCELLED and no step is dispatched (§4.11's "the goroutine that
// starts afterwards meets the step guard" was true only with this write
// conditional).
func TestRunExecution421_EntryWriteDoesNotReopenACancel(t *testing.T) {
	const taskID = "t-421-entry"
	e, rt, tr, er := twoStepExecutor(t, taskID, persistence.TaskStatusRunning)
	rt.onStart = func(*runtime.ContainerConfig) {}
	e.execRepo = &cancelOnExecRunning{MockExecRepo: er, tr: tr, taskID: taskID}

	require.NoError(t, e.Execute(taskID))
	waitIdle(t, e, taskID)

	assert.Equal(t, 0, rt.starts(), "no container may start for a task cancelled before the run marked it RUNNING")
	got, err := tr.Get(context.Background(), taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status)
}

// failSnapshotRepo fails the snapshot write Resume makes after its gate.
type failSnapshotRepo struct{ *MockExecRepo }

func (f *failSnapshotRepo) SaveStateSnapshot(context.Context, string, []byte, string, []string) error {
	return assert.AnError
}

// A write after the gate that fails puts the task back to the status the gate
// moved it from, and starts nothing (code review 2026-10-17 cf90 F3).
func TestResume421_FailedSnapshotWritePutsTheTaskBack(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusPaused, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusPaused}
	require.NoError(t, er.Create(context.Background(), exec))
	require.NoError(t, e.saveExecutionState(context.Background(), exec, executionState{PausedReason: PauseReasonOperator}))
	e.execRepo = &failSnapshotRepo{MockExecRepo: er}

	_, err := e.Resume("t")
	require.Error(t, err)
	assert.False(t, e.IsExecuting("t"))
	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusPaused, got.Status, "the gate's move is undone")
	x, _ := er.Get(context.Background(), "x")
	assert.Equal(t, persistence.ExecutionStatusPaused, x.Status)
}

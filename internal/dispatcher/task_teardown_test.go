package dispatcher

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running. The
// chat cancel_task tool (05-scheduler.md §4.11 site #5) had the same gap: it
// flipped the row and never asked the executor to stop the container. The
// dispatcher now hands the wired executor to the cancel as its teardown.

type dispatcherTeardownSpy struct{ calls []string }

func (s *dispatcherTeardownSpy) CancelIfActive(id string) (bool, error) {
	s.calls = append(s.calls, "CancelIfActive:"+id)
	return true, nil
}
func (s *dispatcherTeardownSpy) NotifyChildTerminal(_ context.Context, id string) {
	s.calls = append(s.calls, "NotifyChildTerminal:"+id)
}
func (s *dispatcherTeardownSpy) CancelChildren(_ context.Context, id string) {
	s.calls = append(s.calls, "CancelChildren:"+id)
}

func TestCancelTask_TearsDownThroughTheWiredExecutor(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
	}
	confirms := newFakeConfirmRepo(nil)
	seedTaskAck(confirms, scopeCancelTask, "t1", testMemOperator, time.Now().Add(time.Hour))
	te := taskExecutor(confirms, repo)
	spy := &dispatcherTeardownSpy{}
	te.taskTeardown = spy

	te.cancelTask(sharedCtx(), `{"task_id":"t1"}`, nil)

	if len(spy.calls) != 3 || spy.calls[0] != "CancelIfActive:t1" {
		t.Fatalf("teardown calls = %v, want CancelIfActive, NotifyChildTerminal, CancelChildren for t1", spy.calls)
	}
}

// The setter must write through: NewAgent copies its fields into the tool
// executor once, so a setter that only set the agent field would leave the
// tool with no teardown (the SetProblemReportBuilder lesson).
func TestSetTaskTeardown_WritesThroughToTheToolExecutor(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil)
	spy := &dispatcherTeardownSpy{}
	a.SetTaskTeardown(spy)
	if a.toolExecutor.taskTeardown != spy {
		t.Fatal("SetTaskTeardown did not reach the tool executor")
	}
}

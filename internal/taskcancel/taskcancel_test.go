package taskcancel

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running.
// Cancelling is the conditional write and then three obligations, and every
// site that skipped one of them had its own copy of the sequence. These tests
// pin the one copy: order, gating on the write, and how failures surface.

type spyTeardown struct {
	calls       []string
	hadExec     bool
	teardownErr error
	ctxErr      error // ctx.Err() seen by the cascade calls
}

func (s *spyTeardown) CancelIfActive(taskID string) (bool, error) {
	s.calls = append(s.calls, "CancelIfActive:"+taskID)
	return s.hadExec, s.teardownErr
}

func (s *spyTeardown) NotifyChildTerminal(ctx context.Context, id string) {
	s.ctxErr = ctx.Err()
	s.calls = append(s.calls, "NotifyChildTerminal:"+id)
}

func (s *spyTeardown) CancelChildren(ctx context.Context, id string) {
	s.ctxErr = ctx.Err()
	s.calls = append(s.calls, "CancelChildren:"+id)
}

func TestCancel_TransitionThenTeardownInOrder(t *testing.T) {
	var order []string
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) {
			order = append(order, "transition")
			return true, nil
		},
	}
	td := &spyTeardown{hadExec: true}

	out, err := Cancel(context.Background(), repo, td, "t1", nil)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !out.Transitioned || !out.TornDown || out.TeardownErr != nil {
		t.Fatalf("outcome = %+v, want transitioned and torn down", out)
	}
	order = append(order, td.calls...)
	want := []string{"transition", "CancelIfActive:t1", "NotifyChildTerminal:t1", "CancelChildren:t1"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// A row that did not move owes nothing: its own terminal transition owes the
// unblock, and there is nothing of ours to tear down (05-scheduler §4.10).
func TestCancel_RefusedTransitionTearsNothingDown(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) { return false, nil },
	}
	td := &spyTeardown{hadExec: true}

	out, err := Cancel(context.Background(), repo, td, "t1", nil)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if out.Transitioned || out.TornDown {
		t.Fatalf("outcome = %+v, want nothing", out)
	}
	if len(td.calls) != 0 {
		t.Fatalf("a refused transition must call no teardown; got %v", td.calls)
	}
}

func TestCancel_WriteErrorReturnsAndTearsNothingDown(t *testing.T) {
	boom := errors.New("db down")
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) { return false, boom },
	}
	td := &spyTeardown{}

	_, err := Cancel(context.Background(), repo, td, "t1", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if len(td.calls) != 0 {
		t.Fatalf("no teardown after a failed write; got %v", td.calls)
	}
}

// A container we could not stop is still running while its row says
// CANCELLED: that must reach the caller, but it is not a failure of the
// cancel itself — the row is canonical.
func TestCancel_TeardownFailureIsReportedNotReturned(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) { return true, nil },
	}
	stopErr := errors.New("podman socket timeout")
	td := &spyTeardown{hadExec: true, teardownErr: stopErr}

	out, err := Cancel(context.Background(), repo, td, "t1", nil)
	if err != nil {
		t.Fatalf("a teardown failure is not a cancel failure: %v", err)
	}
	if out.TornDown || !errors.Is(out.TeardownErr, stopErr) {
		t.Fatalf("outcome = %+v, want TornDown=false and the stop error", out)
	}
	if len(td.calls) != 3 {
		t.Fatalf("the cascade still runs after a failed stop; got %v", td.calls)
	}
}

// An explicit from-set goes through TransitionConditional with exactly that
// set; nil keeps TransitionToCancelled's own narrower list.
func TestCancel_ExplicitFromSetUsesConditionalWrite(t *testing.T) {
	var gotFrom []persistence.TaskStatus
	var gotTo persistence.TaskStatus
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			gotFrom, gotTo = from, to
			return true, nil
		},
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) {
			t.Fatal("an explicit from-set must not use TransitionToCancelled")
			return false, nil
		},
	}
	from := persistence.CancellableTaskStatuses()

	out, err := Cancel(context.Background(), repo, &spyTeardown{}, "t1", from)
	if err != nil || !out.Transitioned {
		t.Fatalf("Cancel: out=%+v err=%v", out, err)
	}
	if gotTo != persistence.TaskStatusCancelled || len(gotFrom) != len(from) {
		t.Fatalf("conditional write from=%v to=%s", gotFrom, gotTo)
	}
}

// The request that asked for the cancel may hang up as soon as the row moved;
// the cascade must not be abandoned with it.
func TestCancel_CascadeSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) {
			cancel() // the caller goes away right after the write
			return true, nil
		},
	}
	td := &spyTeardown{}

	if _, err := Cancel(ctx, repo, td, "t1", nil); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if td.ctxErr != nil {
		t.Fatalf("the cascade ran on a cancelled context: %v", td.ctxErr)
	}
}

// No executor wired: the write still happens and nothing panics.
func TestCancel_NilTeardownWritesOnly(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) { return true, nil },
	}
	out, err := Cancel(context.Background(), repo, nil, "t1", nil)
	if err != nil || !out.Transitioned || out.TornDown {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

type conditionalOnlyRepo struct{ moved bool }

func (r conditionalOnlyRepo) TransitionConditional(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
	return r.moved, nil
}

// A nil from-set means TransitionToCancelled's own list; a repository that
// cannot do that write is a wiring error, reported rather than guessed at.
func TestCancel_NilFromSetNeedsTransitionToCancelled(t *testing.T) {
	td := &spyTeardown{}
	if _, err := Cancel(context.Background(), conditionalOnlyRepo{moved: true}, td, "t1", nil); err == nil {
		t.Fatal("want an error for a nil from-set on a repository without TransitionToCancelled")
	}
	if len(td.calls) != 0 {
		t.Fatalf("no teardown without a write; got %v", td.calls)
	}
}

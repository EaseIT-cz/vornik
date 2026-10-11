// Package taskcancel is the one cancel primitive: the conditional CANCELLED
// write, then — only when that write moved the row — tearing down the live
// execution, unblocking a waiting parent and cascading to children.
//
// Every surface that cancels a task on an operator's or a client's behalf
// calls Cancel: the REST handler, the companion MCP tool, the UI task and
// execution cancels, the chat cancel_task action, Telegram /cancel and the
// CPC timeout cascade. Before it existed each site carried its own copy of
// the sequence, and four of them dropped the teardown (GitHub #300,
// 2026-10-08: a companion cancel left the container running, the next step
// dispatched and the scheduler slot held). Design of record:
// https://docs.vornik.io §4.11.
//
// The package imports only persistence, so every caller can reach it: chat
// cannot import executor (executor imports chat).
package taskcancel

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Teardown is what a cancel owes once the row moved. *executor.Executor
// implements it.
type Teardown interface {
	// CancelIfActive stops the live execution for taskID if the executor is
	// running one. (false, nil) is routine — nothing to stop; (true, err) is
	// a container still running that could not be stopped (§4.7).
	CancelIfActive(taskID string) (hadExecution bool, err error)
	// NotifyChildTerminal drives the parent-unblock sweep; a no-op for a task
	// with no parent.
	NotifyChildTerminal(ctx context.Context, childTaskID string)
	// CancelChildren cancels the task's non-terminal in-project children.
	CancelChildren(ctx context.Context, parentTaskID string)
}

// Repo is the write Cancel needs. TransitionToCancelled is also used, when
// the repository has it, for a nil from-set.
type Repo interface {
	TransitionConditional(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error)
}

type cancelledTransitioner interface {
	TransitionToCancelled(ctx context.Context, id string) (bool, error)
}

// Outcome reports what one Cancel call did.
type Outcome struct {
	// Transitioned: THIS call moved the row to CANCELLED. When false nothing
	// else ran — the row was already finished, and its own terminal
	// transition owes the parent unblock (§4.10).
	Transitioned bool
	// TornDown: a live execution was found and stopped.
	TornDown bool
	// TeardownErr: a live execution was found and stopping it failed. The
	// container may still be running while the row says CANCELLED. Not an
	// error of Cancel itself — the row is canonical — so callers log it.
	TeardownErr error
}

// postWriteTimeout bounds the work done after the write on the detached
// context, so a wedged repository cannot hold a caller forever.
const postWriteTimeout = 30 * time.Second

// ErrNoCancelledTransition reports a nil from-set on a repository that has no
// TransitionToCancelled — a wiring error, reported rather than guessed at.
var ErrNoCancelledTransition = errors.New("taskcancel: a nil from-set needs a repository with TransitionToCancelled")

// Cancel performs the conditional CANCELLED write and, only when it moved the
// row, CancelIfActive, NotifyChildTerminal and CancelChildren, in that order.
//
// from is the set of statuses the caller is willing to cancel. nil keeps
// TransitionToCancelled's own list (QUEUED, LEASED, RUNNING, PENDING, PAUSED),
// which the REST, companion and Telegram surfaces use (§4.10 kept it narrow on
// purpose); otherwise the write is TransitionConditional(from).
//
// td may be nil (no executor wired): the write still happens, and the
// executor's step guard is what then stops further work.
//
// The post-write calls run on a context detached from ctx: a client that hangs
// up after the row moved must not abandon the teardown.
func Cancel(ctx context.Context, repo Repo, td Teardown, taskID string, from []persistence.TaskStatus) (Outcome, error) {
	var (
		moved bool
		err   error
	)
	if from == nil {
		tc, ok := repo.(cancelledTransitioner)
		if !ok {
			return Outcome{}, ErrNoCancelledTransition
		}
		moved, err = tc.TransitionToCancelled(ctx, taskID)
	} else {
		moved, err = repo.TransitionConditional(ctx, taskID, from,
			persistence.TaskStatusCancelled, persistence.TransitionOpts{})
	}
	if err != nil || !moved {
		return Outcome{}, err
	}
	out := Outcome{Transitioned: true}
	if td == nil {
		return out, nil
	}
	had, terr := td.CancelIfActive(taskID)
	out.TornDown = had && terr == nil
	if had && terr != nil {
		out.TeardownErr = terr
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postWriteTimeout)
	defer cancel()
	td.NotifyChildTerminal(postCtx, taskID)
	td.CancelChildren(postCtx, taskID)
	return out, nil
}

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
	"fmt"
	"slices"
	"strings"
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

// narrowStatuses is TransitionToCancelled's from-set, the one a nil from
// means (both drivers' SQL IN list). Used only to phrase a refusal.
var narrowStatuses = []persistence.TaskStatus{
	persistence.TaskStatusPending, persistence.TaskStatusQueued, persistence.TaskStatusLeased,
	persistence.TaskStatusRunning, persistence.TaskStatusPaused,
}

// RefusalReason phrases a cancel whose write did not move the row, from the
// row's CURRENT status — re-read after the refused write, never the snapshot
// taken before it (05-scheduler §4.10 F3; §4.11 "Amended 2026-10-12", GitHub
// #326: the companion reply said "already terminal" for a parked task the
// narrow set excludes). from is the set the caller passed to Cancel (nil =
// TransitionToCancelled's).
func RefusalReason(current persistence.TaskStatus, from []persistence.TaskStatus) string {
	switch current {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed:
		return fmt.Sprintf("the task is already %s; there is nothing to cancel", current)
	}
	if from == nil {
		from = narrowStatuses
	}
	if slices.Contains(from, current) {
		return fmt.Sprintf("the task's status changed while cancelling (now %s); it was left unchanged — try again", current)
	}
	if current == persistence.TaskStatusAwaitingApproval {
		// Refused on purpose by the client surfaces (operator decision
		// 2026-10-13, GitHub #326, §4.11 "Amended 2026-10-13").
		return fmt.Sprintf("the task is %s, which this cancel does not act on: an approval is answered (approve or reject), not cancelled from here; it was left unchanged", current)
	}
	names := make([]string, len(from))
	for i, s := range from {
		names[i] = string(s)
	}
	list := names[0]
	if n := len(names); n > 1 {
		list = strings.Join(names[:n-1], ", ") + " or " + names[n-1]
	}
	return fmt.Sprintf("the task is %s, which this cancel does not act on (it cancels only %s tasks); it was left unchanged", current, list)
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
// TransitionToCancelled's own list (QUEUED, LEASED, RUNNING, PENDING, PAUSED);
// no production site passes nil since 2026-10-13, when the REST, companion and
// Telegram surfaces moved to persistence.ClientCancellableTaskStatuses (GitHub
// #326). Otherwise the write is TransitionConditional(from).
//
// td may be nil (no executor wired): the write still happens, and the
// executor's step guard is what then stops further work.
//
// The post-write calls run on a context detached from ctx: a client that hangs
// up after the row moved must not abandon the teardown.
func Cancel(ctx context.Context, repo Repo, td Teardown, taskID string, from []persistence.TaskStatus) (Outcome, error) {
	return CancelWith(ctx, repo, td, taskID, from, persistence.TransitionOpts{})
}

// ErrOptsNeedFromSet reports options passed with a nil from-set: the narrow
// TransitionToCancelled write takes none, and dropping them silently would
// lose the reason the caller meant to record.
var ErrOptsNeedFromSet = errors.New("taskcancel: transition options need an explicit from-set")

// CancelWith is Cancel with options for the CANCELLED write — a caller that
// cancels on the system's own initiative records why on the row (LastError,
// LastErrorClass), so the task does not read like an operator's cancel. The
// watchdog's approval timeout is the first such caller (outcome-quality LLD
// §S5 #17, GitHub #142). Options need a non-nil from; see ErrOptsNeedFromSet.
func CancelWith(ctx context.Context, repo Repo, td Teardown, taskID string, from []persistence.TaskStatus, opts persistence.TransitionOpts) (Outcome, error) {
	if from == nil && opts != (persistence.TransitionOpts{}) {
		return Outcome{}, ErrOptsNeedFromSet
	}
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
			persistence.TaskStatusCancelled, opts)
	}
	if err != nil || !moved {
		return Outcome{}, err
	}
	out := AfterTerminal(ctx, td, taskID, true)
	out.Transitioned = true
	return out, nil
}

// AfterTerminal is what a terminal write owes once it moved the row: stop the
// live execution (CancelIfActive), drive the parent unblock
// (NotifyChildTerminal) and, when cascadeChildren is set, cancel the task's
// children. Cancel calls it after its CANCELLED write; the watchdog calls it,
// without the cascade, after its stuck-execution FAILED write (05-scheduler
// §4.12 C3, GitHub #142), so there is one teardown sequence, not two.
//
// Call it only when the caller's own write moved the row. The post-write
// calls run on a context detached from ctx, bounded by postWriteTimeout. td
// nil does nothing. The returned Outcome's Transitioned is false; the caller
// knows whether it transitioned.
func AfterTerminal(ctx context.Context, td Teardown, taskID string, cascadeChildren bool) Outcome {
	var out Outcome
	if td == nil {
		return out
	}
	had, terr := td.CancelIfActive(taskID)
	out.TornDown = had && terr == nil
	if had && terr != nil {
		out.TeardownErr = terr
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postWriteTimeout)
	defer cancel()
	td.NotifyChildTerminal(postCtx, taskID)
	if cascadeChildren {
		td.CancelChildren(postCtx, taskID)
	}
	return out
}

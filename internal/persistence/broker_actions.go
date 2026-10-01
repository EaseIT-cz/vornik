package persistence

import (
	"context"
	"errors"
	"time"
)

// Broker actions — https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md §5.
//
// A broker action is one write a broker workflow proposed. A person approves
// it in /inbox, and the daemon then executes exactly the stored arguments by
// calling the declared MCP tool. No model runs between approval and
// execution, and no agent ever holds the write tool.

// Broker action states. Every transition is a guarded compare-and-set with
// its legal source states in the WHERE clause.
const (
	// BrokerActionStaged: written before the task's COMPLETED transition.
	// Invisible to /inbox and never approvable; replaceable by a retry.
	BrokerActionStaged = "staged"
	// BrokerActionPending: promoted after COMPLETED; shown in /inbox.
	BrokerActionPending   = "pending"
	BrokerActionApproved  = "approved"
	BrokerActionExecuting = "executing"
	BrokerActionExecuted  = "executed"
	BrokerActionFailed    = "failed"
	BrokerActionRejected  = "rejected"
	BrokerActionExpired   = "expired"
	// BrokerActionUnknown: the call may or may not have happened. Only an
	// operator resolves it; nothing retries a write.
	BrokerActionUnknown = "unknown"
	// BrokerActionDiscarded: a staged row whose task never completed.
	BrokerActionDiscarded = "discarded"
	// BrokerActionProposalMissing / Invalid: nothing to approve. Stored as
	// terminal rows so the outcome survives artifact retention.
	BrokerActionProposalMissing = "proposal_missing"
	BrokerActionProposalInvalid = "proposal_invalid"
)

// Outcome classes set by the terminal CAS (design §5.4).
const (
	BrokerOutcomeOK               = "ok"
	BrokerOutcomeToolError        = "tool_error"
	BrokerOutcomePreSendError     = "pre_send_error"
	BrokerOutcomeTimeout          = "timeout"
	BrokerOutcomeTransportError   = "transport_error"
	BrokerOutcomeOperatorResolved = "operator_resolved"
)

// ErrBrokerActionNoTransition is returned by a guarded transition when the
// row was not in a legal source state (or the id does not exist, or, for
// Approve, the shown hash or the expiry no longer matches).
var ErrBrokerActionNoTransition = errors.New("persistence: broker action not in a valid source state for transition")

// BrokerAction is one proposed write.
type BrokerAction struct {
	ActionID   string
	ProjectID  string
	TaskID     string
	APIKeyID   string // the front agent's key that delegated the task
	WorkflowID string
	ActionKind string // the workflow's proposes[].action
	Tool       string // mcp__<server>__<tool>

	// ArgsJSON is the canonical argument bytes and ArgsSHA256 their hash.
	// Written only by Stage, and only while the row is unapprovable: from
	// pending onward they are write-once (design §5.2).
	ArgsJSON   []byte
	ArgsSHA256 string

	Status       string
	Approver     string
	OutcomeJSON  []byte // redacted, capped tool response or operator note
	OutcomeClass string

	CreatedAt time.Time
	ExpiresAt time.Time
	DecidedAt *time.Time
	// ExecutedAt is written by the claim (execution started) and again by
	// the terminal transition. Executing and unknown rows age from it.
	ExecutedAt *time.Time
}

// BrokerActionStuck is one row of the stuck-row gauge (design §5.4).
type BrokerActionStuck struct {
	ProjectID string
	Status    string
	Count     int64
}

// BrokerActionRepository persists broker actions.
type BrokerActionRepository interface {
	// Stage upserts the row for (TaskID, ActionKind). It inserts, or it
	// replaces an existing row only while that row is staged,
	// proposal_missing, proposal_invalid or discarded, none of which was
	// ever shown or can be approved. (discarded: a paused or waiting task's
	// staged rows are discarded, and its proposal must still land when it
	// resumes — review-20260930-ac20.)
	// A row in any other state is left untouched and replaced is false.
	// a.Status must be staged, proposal_missing or proposal_invalid.
	Stage(ctx context.Context, a *BrokerAction) (replaced bool, err error)
	// PromoteStaged moves the task's staged rows to pending (after the
	// task's COMPLETED transition) and returns how many moved.
	PromoteStaged(ctx context.Context, taskID string) (int64, error)
	// PromoteStagedOfCompletedTasks is the startup sweep: staged rows whose
	// task is COMPLETED (a crash between the transition and PromoteStaged).
	PromoteStagedOfCompletedTasks(ctx context.Context) (int64, error)
	// DiscardStagedOrphans marks discarded the staged rows whose task is
	// neither COMPLETED nor running (QUEUED, LEASED, RUNNING).
	DiscardStagedOrphans(ctx context.Context) (int64, error)

	// Get loads one action. ErrNotFound when absent.
	Get(ctx context.Context, actionID string) (*BrokerAction, error)
	// ListByTask returns the task's actions, oldest first.
	ListByTask(ctx context.Context, taskID string) ([]*BrokerAction, error)
	// ListByStatus returns actions in status, oldest first; projectID ""
	// means every project.
	ListByStatus(ctx context.Context, projectID, status string, limit int) ([]*BrokerAction, error)

	// Approve is pending→approved, guarded by the hash the approver was
	// shown and by expiry. ErrBrokerActionNoTransition otherwise.
	Approve(ctx context.Context, actionID, shownArgsSHA256, approver string, now time.Time) error
	// Reject is (pending|approved)→rejected.
	Reject(ctx context.Context, actionID, approver string, now time.Time) error
	// ClaimForExecution is approved→executing; true only for the one winner.
	ClaimForExecution(ctx context.Context, actionID string, now time.Time) (bool, error)
	// Finish is executing→(executed|failed|unknown), with the outcome.
	Finish(ctx context.Context, actionID, status, outcomeClass string, outcome []byte, now time.Time) error
	// Resolve is the operator's (executing|unknown)→(executed|failed); it
	// never re-executes. outcome_class becomes operator_resolved.
	Resolve(ctx context.Context, actionID, status, approver string, note []byte, now time.Time) error
	// ExpireDue moves pending and approved rows past expires_at to expired.
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
	// CountStuck feeds the stuck-row gauge: executing/unknown rows older
	// than stuckAfter and approved rows older than approvedAfter, by
	// project and status.
	CountStuck(ctx context.Context, now time.Time, stuckAfter, approvedAfter time.Duration) ([]BrokerActionStuck, error)
}

// BrokerActionStageable reports whether s is a status Stage may write.
func BrokerActionStageable(s string) bool {
	switch s {
	case BrokerActionStaged, BrokerActionProposalMissing, BrokerActionProposalInvalid:
		return true
	}
	return false
}

// Stuck-row thresholds (design §5.4). Executing rows age from the claim and
// unknown rows from the call's end (executed_at), so a stuck executing row
// is one whose call cannot still be in flight: BrokerActionStuckAfter
// exceeds the longest broker.action_timeout (10m). Approved rows age from
// the approval (decided_at). review-20260930-1334 F1.
const (
	BrokerActionStuckAfter         = 15 * time.Minute
	BrokerActionApprovedStuckAfter = 5 * time.Minute
)

// BrokerActionIsStuck is CountStuck's predicate for one row, for callers
// that need the rows themselves (the doctor check lists their ids).
func BrokerActionIsStuck(a *BrokerAction, now time.Time) bool {
	if a == nil {
		return false
	}
	since := BrokerActionAgeSince(a)
	switch a.Status {
	case BrokerActionExecuting, BrokerActionUnknown:
		return !since.After(now.Add(-BrokerActionStuckAfter))
	case BrokerActionApproved:
		return a.DecidedAt != nil && !a.DecidedAt.After(now.Add(-BrokerActionApprovedStuckAfter))
	}
	return false
}

// BrokerActionAgeSince is the instant a row's stuck age counts from, as the
// stores' CountStuck computes it: executed_at, else decided_at, else
// created_at. For an approved row that is the approval.
func BrokerActionAgeSince(a *BrokerAction) time.Time {
	switch {
	case a.ExecutedAt != nil:
		return *a.ExecutedAt
	case a.DecidedAt != nil:
		return *a.DecidedAt
	}
	return a.CreatedAt
}

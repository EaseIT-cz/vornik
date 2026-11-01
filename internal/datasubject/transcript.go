package datasubject

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Slice 5d-1: erasing one participant from a shared transcript (GitHub #344).
//
// A shared chat_audit_log or task_messages row is not rewritten by a model
// (O-5d-1). The subject's own turns are tombstoned and every identifier of
// theirs in the other participants' turns is masked, deterministically. These
// are the types the engine (applyTranscriptRedact) and the store
// (subjectstore's TranscriptRedactor) share.
//
// see LLD § https://docs.vornik.io §15.14

// TranscriptTurn is one transcript row as loaded, the version every write is
// guarded on.
type TranscriptTurn struct {
	Table LinkableTable
	RowID string
	// Author is task_messages.author_id or chat_audit_log.user_id ("" for NULL).
	Author string
	// ChatID is chat_audit_log.chat_id; empty for task_messages.
	ChatID string
	// ThreadKey is what ThreadAuthors orders within: the task id or the chat id.
	ThreadKey string
	// TaskID is task_messages.task_id, the live anchor of a task message.
	TaskID string
	// PromptHash and PromptBody are chat_audit_log's system prompt.
	PromptHash string
	PromptBody *string
	// Fields holds every content column; nil is SQL NULL, never "".
	Fields map[string]*string
	SentAt time.Time
}

// ThreadAuthor is one distinct author of a thread with its first turn.
type ThreadAuthor struct {
	Author  string
	FirstAt time.Time
	FirstID string
}

// PromptRekey moves a turn to a masked copy of its system prompt.
type PromptRekey struct {
	OldHash string
	NewBody string
}

// TurnWrite is the replacement for one turn. Fields carries every content
// column; nil writes NULL.
type TurnWrite struct {
	Fields map[string]*string
	Author string
	// ChatID is set only to pseudonymise a DM chat id (§15.11.1 item 5).
	ChatID *string
	Prompt *PromptRekey
}

// TranscriptOutcome is what the store did with one write.
type TranscriptOutcome string

const (
	// TranscriptApplied means the guarded UPDATE matched and committed.
	TranscriptApplied TranscriptOutcome = "applied"
	// TranscriptLive means a task in the row's subtree is not settled, so
	// nothing was written. LiveTaskID names it.
	TranscriptLive TranscriptOutcome = "live"
	// TranscriptVersionChanged means a guarded column no longer held the
	// loaded value, so nothing was written.
	TranscriptVersionChanged TranscriptOutcome = "version_changed"
)

// ReferrerDeferral is another row on the old prompt body that was not moved,
// because its task is live or because it appeared during the erasure.
type ReferrerDeferral struct {
	RowID      string
	LiveTaskID string
	// Arrived marks a referrer counted only after the old body's delete: the
	// prompt's source still renders the subject's data (review 0e4e F4).
	Arrived bool
}

// TurnResult is the outcome of ApplyTurn.
type TurnResult struct {
	Outcome    TranscriptOutcome
	LiveTaskID string
	// NewPromptHash is the masked body's hash when the prompt was re-keyed.
	NewPromptHash string
	// Repointed counts other referrers moved to the masked body.
	Repointed int
	// DeferredReferrers are other referrers left on the old body.
	DeferredReferrers []ReferrerDeferral
	// OldPromptDeleted reports whether the old body is gone.
	OldPromptDeleted bool
}

// TaskPayload is one task's payload in an origin turn's subtree.
type TaskPayload struct {
	TaskID  string
	Status  persistence.TaskStatus
	Payload *string
}

// TranscriptRedactor is the store side of 5d-1, on both drivers.
type TranscriptRedactor interface {
	LoadTurn(ctx context.Context, table LinkableTable, rowID string) (TranscriptTurn, error)
	// ThreadAuthors lists the thread's distinct non-empty authors ordered by
	// first turn, ties broken by the smallest row id.
	ThreadAuthors(ctx context.Context, table LinkableTable, threadKey string) ([]ThreadAuthor, error)
	// ApplyTurn writes w over prior in one transaction, guarded on every
	// content and routing column and on the row's task subtree being settled.
	ApplyTurn(ctx context.Context, prior TranscriptTurn, w TurnWrite) (TurnResult, error)
	// OriginPayloads lists every task in the subtree anchored on
	// tasks.chat_turn_id = chatTurnID, through parent_task_id.
	OriginPayloads(ctx context.Context, chatTurnID string) ([]TaskPayload, error)
	// ApplyPayload replaces one task's payload, guarded on the loaded value and
	// on the task's subtree being settled. Its own transaction.
	ApplyPayload(ctx context.Context, prior TaskPayload, payload string) (TurnResult, error)
}

// transcriptLiveStatuses are the task statuses under which a transcript row
// is still model input or still being written, so 5d defers it. COMPLETED is
// settled (O-5d-6). Every persistence.TaskStatus is classified here or in
// transcriptSettledStatuses; a test fails on one that is in neither.
var transcriptLiveStatuses = []persistence.TaskStatus{
	persistence.TaskStatusPending,
	persistence.TaskStatusQueued,
	persistence.TaskStatusLeased,
	persistence.TaskStatusRunning,
	persistence.TaskStatusWaitingForChildren,
	persistence.TaskStatusAwaitingInput,
	persistence.TaskStatusAwaitingExternal,
	persistence.TaskStatusPaused,
	persistence.TaskStatusAwaitingApproval,
}

var transcriptSettledStatuses = []persistence.TaskStatus{
	persistence.TaskStatusCompleted,
	persistence.TaskStatusFailed,
	persistence.TaskStatusCancelled,
	persistence.TaskStatusClosed,
}

// TranscriptLiveStatuses returns the live statuses as strings, for the
// store's SQL predicate.
func TranscriptLiveStatuses() []string {
	out := make([]string, len(transcriptLiveStatuses))
	for i, s := range transcriptLiveStatuses {
		out[i] = string(s)
	}
	return out
}

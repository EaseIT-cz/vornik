package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Chat memory-write design §12 (2026-09-25): cancel_task and retry_task use the §5.3 two-step
// instead of a `confirm` flag the model sets on its own tool call. The tool PROPOSES; the same
// human acknowledges in their own next inbound turn with a closed phrase (stamped in
// ChannelReceiver.Receive, never from a tool argument); the next call for the same task is
// AUTHORIZED once and the row deleted. The pending row lives in the same store as the
// shared-memory confirmation — one per conversation, whatever its kind.

// Confirmation scopes for the two destructive task actions. Also the keys of
// acknowledgementPhraseSets.
const (
	scopeCancelTask = "cancel_task"
	scopeRetryTask  = "retry_task"
)

// taskActionConfirmationTTL bounds how long an authorization stays open — the same window as
// a shared write (§5.3.3): a human answering in a thread they are not watching.
const taskActionConfirmationTTL = sharedConfirmationTTL

// taskActionFingerprint pins a confirmation to ONE action on ONE task, so acknowledging a
// cancel of task A can never authorize cancelling task B, or retrying A.
func taskActionFingerprint(scope, taskID string) string {
	sum := sha256.Sum256([]byte(scope + ":" + strings.TrimSpace(taskID)))
	return hex.EncodeToString(sum[:])
}

// authorizeTaskAction is the decision: the pending row is ACKNOWLEDGED by the same speaker,
// for the same action kind on the same task, and has not expired. Everything else refuses.
func authorizeTaskAction(rec *persistence.ChatMemoryWriteConfirmation, scope, taskID, operatorID string, now time.Time) bool {
	return rec != nil && operatorID != "" &&
		rec.Scope == scope &&
		rec.OperatorID == operatorID &&
		rec.Acknowledged() &&
		rec.ContentFingerprint == taskActionFingerprint(scope, taskID) &&
		now.Before(rec.ExpiresAt)
}

// confirmTaskAction runs the two-step for one destructive call. It returns (true, _) when the
// action is authorized — the row has been deleted and the caller may act exactly once — and
// otherwise (false, reply), where reply is what the model must relay. Every refusal returns
// HERE, before any chat.Action is built, so ExecuteAction's Confirm flag is only ever set on an
// authorized path.
func (te *ToolExecutor) confirmTaskAction(ctx context.Context, scope, verb, taskID string) (bool, ToolResult) {
	if te.memoryConfirms == nil {
		return false, ToolResult{Content: "I can't " + verb + " a task from chat here: the confirmation step " +
			"that must come from the user is not available on this deployment. Nothing was changed. " +
			"The task can be managed from the web UI or vornikctl."}
	}
	operatorID, _ := operatorIDFromContext(ctx)
	channel, sessionID := originatingChannelFromContext(ctx)
	if operatorID == "" || channel == "" || sessionID == "" {
		return false, ToolResult{Content: "I can't " + verb + " a task here: I can't tell who is asking, and " +
			"this action has to be confirmed by the person who asked for it. Nothing was changed."}
	}

	now := time.Now()
	rec, err := te.memoryConfirms.Get(ctx, channel, sessionID)
	if errors.Is(err, persistence.ErrNotFound) {
		rec = nil
	} else if err != nil {
		return false, ToolResult{Content: "I couldn't check the confirmation state for this conversation, so " +
			"nothing was changed. Tell the user there was a temporary problem and to try again."}
	}

	if authorizeTaskAction(rec, scope, taskID, operatorID, now) {
		// One-shot: consume the authorization BEFORE acting, so a failed action cannot leave
		// it reusable. The user confirms again if they want another attempt.
		if err := te.memoryConfirms.Delete(ctx, channel, sessionID); err != nil {
			return false, ToolResult{Content: "I couldn't consume the confirmation, so nothing was changed. " +
				"Tell the user there was a temporary problem and to try again."}
		}
		return true, ToolResult{}
	}

	if err := te.memoryConfirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
		Channel:            channel,
		SessionID:          sessionID,
		ContentFingerprint: taskActionFingerprint(scope, taskID),
		Scope:              scope,
		OperatorID:         operatorID,
		ProposedAt:         now,
		ExpiresAt:          now.Add(taskActionConfirmationTTL),
	}); err != nil {
		return false, ToolResult{Content: "I couldn't record the confirmation for this, so nothing was changed. " +
			"Tell the user there was a temporary problem."}
	}
	quoted := make([]string, 0, len(acknowledgementPhrasesFor(scope)))
	for _, p := range acknowledgementPhrasesFor(scope) {
		quoted = append(quoted, `"`+p+`"`)
	}
	return false, ToolResult{Content: "Confirmation required: nothing has been done yet. To " + verb + " task " +
		taskID + ", the user must reply with exactly one of these phrases: " + strings.Join(quoted, ", ") +
		". Relay this to the user and wait for them to type it; then call this tool again for the same task. " +
		"Do not claim the task was " + verb + "ed, and do not ask them to just say yes."}
}

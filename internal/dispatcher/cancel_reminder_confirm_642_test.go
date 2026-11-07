package dispatcher

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #642 (incident 2026-11-04 22:47:56): inside a looping turn the
// dispatcher called cancel_reminder, and it ran at once on the model's word.
// cancel_task already used the chat memory-write design §12 two-step;
// cancel_reminder did not. Now it does: the tool proposes, the same human
// acknowledges in their own next turn with a closed phrase, and only the
// next call for that reminder acts, once.
//
// Found at: production (incident report /tmp/regression.md, defect 5).
// Regression test at: unit, the tool executor's own cancel_reminder path.
func reminderConfirmExecutor(confirms *fakeConfirmRepo) (*ToolExecutor, *reminderRepoForTools) {
	repo := newReminderRepoForTools()
	repo.rows["rem_1"] = &persistence.Reminder{ID: "rem_1", OperatorID: testMemOperator, Status: persistence.ReminderStatusPending}
	repo.rows["rem_2"] = &persistence.Reminder{ID: "rem_2", OperatorID: testMemOperator, Status: persistence.ReminderStatusPending}
	te := &ToolExecutor{reminderRepo: repo}
	if confirms != nil {
		te.memoryConfirms = confirms
	}
	return te, repo
}

func cancelReminderCall(id string) chat.ToolCall {
	return chat.ToolCall{Function: chat.FunctionCall{Name: "cancel_reminder",
		Arguments: `{"reminder_id":"` + id + `","rationale":"r"}`}}
}

func TestCancelReminder_ProposesFirst_642(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	te, repo := reminderConfirmExecutor(confirms)
	res := te.Execute(sharedCtx(), cancelReminderCall("rem_1"), "", nil, 0, "", nil)
	if len(repo.cancels) != 0 {
		t.Fatalf("the reminder was cancelled on the model's word: %v", repo.cancels)
	}
	row, ok := confirms.get(testMemChannel)
	if !ok || row.Scope != scopeCancelReminder || row.Acknowledged() {
		t.Fatalf("no pending cancel_reminder proposal: ok=%v row=%+v", ok, row)
	}
	if !strings.Contains(res.Content, "Confirmation required") || !strings.Contains(res.Content, "reminder rem_1") {
		t.Fatalf("the reply must ask the user to confirm this reminder: %q", res.Content)
	}
}

func TestCancelReminder_AcknowledgedActsOnceForThatReminder_642(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	seedTaskAck(confirms, scopeCancelReminder, "rem_1", testMemOperator, time.Now().Add(10*time.Minute))
	te, repo := reminderConfirmExecutor(confirms)
	// An acknowledgement for rem_1 cannot cancel rem_2.
	te.Execute(sharedCtx(), cancelReminderCall("rem_2"), "", nil, 0, "", nil)
	if len(repo.cancels) != 0 {
		t.Fatalf("an acknowledgement for rem_1 cancelled %v", repo.cancels)
	}
	seedTaskAck(confirms, scopeCancelReminder, "rem_1", testMemOperator, time.Now().Add(10*time.Minute))
	te.Execute(sharedCtx(), cancelReminderCall("rem_1"), "", nil, 0, "", nil)
	if len(repo.cancels) != 1 || repo.cancels[0] != "rem_1" {
		t.Fatalf("the acknowledged cancel did not act once: %v", repo.cancels)
	}
	te.Execute(sharedCtx(), cancelReminderCall("rem_1"), "", nil, 0, "", nil)
	if len(repo.cancels) != 1 {
		t.Fatalf("a second call acted without a new acknowledgement: %v", repo.cancels)
	}
}

// An acknowledgement of any other scope cannot cancel a reminder (review
// 0df0 F2).
func TestCancelReminder_OtherScopesDoNotCount_642(t *testing.T) {
	for _, scope := range []string{scopeCancelTask, scopeRetryTask, string(memoryScopeShared)} {
		confirms := newFakeConfirmRepo(nil)
		seedTaskAck(confirms, scope, "rem_1", testMemOperator, time.Now().Add(10*time.Minute))
		te, repo := reminderConfirmExecutor(confirms)
		te.Execute(sharedCtx(), cancelReminderCall("rem_1"), "", nil, 0, "", nil)
		if len(repo.cancels) != 0 {
			t.Fatalf("a %s acknowledgement cancelled a reminder", scope)
		}
	}
}

// Ownership is checked before any proposal is written, so another operator's
// reminder neither cancels nor gets a pending row (review 0df0 F1).
func TestCancelReminder_OtherOperatorGetsNoProposal_642(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	te, repo := reminderConfirmExecutor(confirms)
	repo.rows["rem_other"] = &persistence.Reminder{ID: "rem_other", OperatorID: "telegram:99", ChannelRef: "99", Status: persistence.ReminderStatusPending}
	res := te.Execute(sharedCtx(), cancelReminderCall("rem_other"), "", nil, 0, "", nil)
	if len(repo.cancels) != 0 {
		t.Fatal("another operator's reminder was cancelled")
	}
	if _, ok := confirms.get(testMemChannel); ok {
		t.Fatal("a proposal was recorded for another operator's reminder")
	}
	if !strings.Contains(res.Content, "not yours") {
		t.Fatalf("expected the ownership refusal, got %q", res.Content)
	}
}

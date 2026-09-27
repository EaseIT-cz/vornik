package dispatcher

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// Chat memory-write design §12: one pending confirmation per conversation can
// now be a shared-memory write, a cancel or a retry, so an acknowledgement
// must discharge only a proposal of ITS OWN kind — before this, any share
// phrase stamped whatever row was pending.

func TestAcknowledgementIsScoped(t *testing.T) {
	scopes := []string{string(memoryScopeShared), scopeCancelTask, scopeRetryTask}
	for _, scope := range scopes {
		phrases := acknowledgementPhrasesFor(scope)
		if len(phrases) == 0 {
			t.Fatalf("scope %q has no phrases", scope)
		}
		for _, p := range phrases {
			for _, other := range scopes {
				if got := acknowledgementMatchesScope(p, other); got != (other == scope) {
					t.Errorf("phrase %q (scope %s) matches scope %s = %v", p, scope, other, got)
				}
			}
		}
	}
	for _, s := range []string{"yes", "ano", "ok", "cancel", "please cancel it now"} {
		for _, scope := range scopes {
			if acknowledgementMatchesScope(s, scope) {
				t.Errorf("%q must not acknowledge %s", s, scope)
			}
		}
	}
	if acknowledgementMatchesScope("cancel it", "some_future_scope") {
		t.Error("an unknown scope must acknowledge nothing")
	}
	// Normalisation matches §5.3.3: case, whitespace, trailing punctuation.
	if !acknowledgementMatchesScope("  Cancel   it! ", scopeCancelTask) || !acknowledgementMatchesScope("zrus to.", scopeCancelTask) {
		t.Error("normalised cancel phrases must match")
	}
}

func seedPendingScope(repo *fakeConfirmRepo, scope string) {
	now := time.Now()
	_ = repo.Propose(context.Background(), &persistence.ChatMemoryWriteConfirmation{
		Channel: testMemChannel, SessionID: testMemSession, ContentFingerprint: "fp",
		Scope: scope, OperatorID: testMemOperator, ProposedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	})
}

func TestReceiver_AcknowledgementDischargesOnlyItsOwnScope(t *testing.T) {
	for _, tc := range []struct {
		pending, text string
		acked         bool
	}{
		{scopeCancelTask, "share it", false},
		{scopeCancelTask, "cancel it", true},
		{scopeCancelTask, "retry it", false},
		{scopeRetryTask, "retry it", true},
		{string(memoryScopeShared), "cancel it", false},
		{string(memoryScopeShared), "share it", true},
	} {
		repo := newFakeConfirmRepo(nil)
		seedPendingScope(repo, tc.pending)
		rcv := receiverWithConfirms(repo)
		if err := rcv.Receive(context.Background(), conversation.ChannelMessage{
			Source: testMemChannel, SessionID: testMemSession, SpeakerID: "UALICE", Text: tc.text,
		}); err != nil {
			t.Fatalf("Receive: %v", err)
		}
		row, _ := repo.get(testMemChannel)
		if row.Acknowledged() != tc.acked {
			t.Errorf("pending %s, typed %q: acknowledged=%v, want %v", tc.pending, tc.text, row.Acknowledged(), tc.acked)
		}
	}
}

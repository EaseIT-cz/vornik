package github

// GitHub #369 §S3 #21 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 4): a failed @mention turn that posted nothing tells the thread once
// an hour. Found at: review (the #142 audit); observed at: production (a
// thread). Test at: integration, the channel against an httptest GitHub.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFailureNotice369_AFailedTurnTellsTheThreadOnce(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)
	bindReceiver(ch, &replyThenFailReceiver{ch: ch, reply: false})
	body := []byte(`{"action":"created","repository":{"full_name":"acme/api"},"sender":{"login":"vadim"},"installation":{"id":99},"issue":{"number":3,"title":"q"},"comment":{"id":77,"body":"@vornik-companion hi","user":{"login":"vadim"}}}`)
	for i := range 3 {
		ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", fmt.Sprintf("d-%d", i), body))
		ch.waitInFlight()
	}
	if n := stub.commentCalls.Load(); n != 1 {
		t.Fatalf("comments posted = %d, want 1: one notice per thread per hour", n)
	}
	// The notice speaks as the assistant, so it carries the Art 50 notice
	// (gate 2026-11-18: the outbound law found it sent bare).
	if !strings.Contains(stub.lastCommentBody, "AI system") {
		t.Fatalf("the failure notice carries no AI disclosure: %s", stub.lastCommentBody)
	}
}

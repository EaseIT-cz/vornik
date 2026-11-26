package configassist

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #922 (bench arm C, 2026-11-25 12:57): glm-5.3 at reasoning medium
// was cut off at 16384 completion tokens before it answered, and the healing
// generate call failed ASSISTANT_REPLY_TRUNCATED. The loop now retries that
// call once with the reasoning effort one level lower, and refuses only if
// the retry is cut off too.
//
// Found at: production (bench arm, the healing generate endpoint's 502).
// Regression test at: unit (the engine over a scripted provider).
// Lower because: the cut-off needs a live reasoning model; the loop's
// reaction to it is fixed by one scripted response.

func TestPropose_922_TruncationRetriesOnceWithLessReasoning(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.assistant.script = append([]*chat.ChatResponse{truncatedResp("glm-5.3")}, f.assistant.script...)
	f.scriptJudge("pass", "cadence: 2h")
	p := &ctxProvider{scriptedProvider: f.assistant}
	f.engine.Assistant = p
	f.cfg.Reasoning = "medium"
	res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("a retry at lower reasoning must let the proposal through: %+v", res.Refusal)
	}
	if len(p.efforts) < 2 || p.efforts[0] != "medium" || p.efforts[1] != "low" {
		t.Fatalf("efforts = %v, want medium then low", p.efforts)
	}
	// The cut-off reply is not added to the conversation: the retry sends
	// the same messages as the first call.
	if len(f.assistant.messages) < 2 || len(f.assistant.messages[0]) != len(f.assistant.messages[1]) {
		t.Fatalf("the retry must resend the same conversation: %d then %d messages", len(f.assistant.messages[0]), len(f.assistant.messages[1]))
	}
	for _, e := range p.efforts[1:] {
		if e != "low" {
			t.Fatalf("the lowered effort must hold for the rest of the request: %v", p.efforts)
		}
	}
}

func TestPropose_922_SecondTruncationRefusesAndSaysItRetried(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{truncatedResp("glm-5.3"), truncatedResp("glm-5.3")}
	p := &ctxProvider{scriptedProvider: f.assistant}
	f.engine.Assistant = p
	f.cfg.Reasoning = "low"
	res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseTruncated {
		t.Fatalf("refusal = %+v, want %s", res.Refusal, RefuseTruncated)
	}
	if !strings.Contains(res.Refusal.Message, "retried at reasoning off") {
		t.Fatalf("message %q does not say it retried", res.Refusal.Message)
	}
	if len(p.efforts) != 2 || p.efforts[1] != "off" {
		t.Fatalf("efforts = %v, want low then off", p.efforts)
	}
}

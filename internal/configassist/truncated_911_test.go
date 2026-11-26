package configassist

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #911 (found in the #809 bench arm C, 2026-11-24): on both healing
// triggers glm-5.3 spent its whole 16384-token output budget reasoning and
// returned an empty reply with finish_reason=length. The loop took that as
// the assistant's final answer, the engine saw no edit, and the request was
// refused NO_CHANGE with an empty reason: "the assistant made no change: ".
//
// Found at: production (bench arm, the healing generate endpoint's 502).
// Regression test at: unit (the engine over a scripted provider).
// Lower because: the defect is the loop's reading of one response; the
// bench run that showed it needs a live reasoning model.

func truncatedResp(model string) *chat.ChatResponse {
	r := textResp(model, "")
	r.Choices[0].FinishReason = "length"
	r.Usage.CompletionTokens = 16384
	return r
}

func TestPropose_911_TruncatedReplyIsNotNoChange(t *testing.T) {
	f := newFixture(t)
	// Twice: since #922 the first cut-off is retried once at lower reasoning.
	f.assistant.script = []*chat.ChatResponse{truncatedResp("glm-5.2:cloud"), truncatedResp("glm-5.2:cloud")}
	res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseTruncated {
		t.Fatalf("refusal = %+v, want %s", res.Refusal, RefuseTruncated)
	}
	for _, want := range []string{"output-token limit", "16384", "retried at reasoning low", "max_tokens"} {
		if !strings.Contains(res.Refusal.Message, want) {
			t.Fatalf("message %q lacks %q", res.Refusal.Message, want)
		}
	}
}

// A truncated reply that still carries tool calls is not final: the loop
// runs the calls as before (only a reply with no tool calls is an answer).
func TestPropose_911_TruncatedMidPlanAfterAnEditStillRefuses(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	// Twice: since #922 the first cut-off is retried once at lower reasoning.
	f.assistant.script[1] = truncatedResp("glm-5.2:cloud")
	f.assistant.script = append(f.assistant.script[:2], append([]*chat.ChatResponse{truncatedResp("glm-5.2:cloud")}, f.assistant.script[2:]...)...)
	res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseTruncated {
		t.Fatalf("refusal = %+v, want %s", res.Refusal, RefuseTruncated)
	}
}

func TestPropose_911_NoChangeSaysWhy(t *testing.T) {
	for _, tc := range []struct{ reply, want string }{
		{"", "the assistant made no change and gave no reason"},
		{"The failures are flaky tests, not configuration.", "the assistant made no change: The failures are flaky tests, not configuration."},
	} {
		f := newFixture(t)
		f.assistant.script = []*chat.ChatResponse{textResp("glm-5.2:cloud", tc.reply)}
		res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Refusal == nil || res.Refusal.Code != RefuseNoChange || res.Refusal.Message != tc.want {
			t.Fatalf("refusal = %+v, want NO_CHANGE %q", res.Refusal, tc.want)
		}
	}
}

// ctxProvider records the reasoning effort each call carried.
type ctxProvider struct {
	*scriptedProvider
	efforts []string
}

func (p *ctxProvider) CompleteWithTools(ctx context.Context, m []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error) {
	p.efforts = append(p.efforts, chat.ReasoningEffortFromContext(ctx))
	return p.scriptedProvider.CompleteWithTools(ctx, m, tools)
}

// config_assistant.reasoning reaches every assistant call; empty leaves the
// model's default (no effort sent).
func TestPropose_911_ReasoningLevelReachesTheAssistant(t *testing.T) {
	for _, level := range []string{"low", ""} {
		f := newFixture(t)
		f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
		f.scriptJudge("pass", "cadence: 2h")
		p := &ctxProvider{scriptedProvider: f.assistant}
		f.engine.Assistant = p
		f.cfg.Reasoning = level
		if _, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent")); err != nil {
			t.Fatal(err)
		}
		if len(p.efforts) == 0 {
			t.Fatal("the assistant was never called")
		}
		for _, got := range p.efforts {
			if got != level {
				t.Fatalf("level %q: a call carried %q", level, got)
			}
		}
	}
}

// The healing adapter returns a refusal as a *RefusalError, whose text keeps
// the form the journal and the bench already read.
func TestRefusalError_911_Text(t *testing.T) {
	e := &RefusalError{Refusal: Refusal{Code: RefuseNoChange, Message: "the assistant made no change and gave no reason"}}
	if got := e.Error(); got != "configassist: healing refused: NO_CHANGE: the assistant made no change and gave no reason" {
		t.Fatalf("Error() = %q", got)
	}
}

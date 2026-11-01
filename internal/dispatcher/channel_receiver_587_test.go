package dispatcher

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// markingAgent is a stubAgent that counts first-answer marks.
type markingAgent struct {
	stubAgent
	marks int
}

func (m *markingAgent) MarkAnswered(context.Context) { m.marks++ }

// GitHub #587 (operations-api-contract-design.md §7.2.2): every channel's
// assistant turn passes ChannelReceiver.receive, so a turn the dispatcher
// answered is the instance's first-answer event there; an errored or empty
// turn is not.
func TestChannelReceiver_MarksAnAnsweredTurn(t *testing.T) {
	cases := []struct {
		name   string
		result Result
		want   int
	}{
		{"answered", Result{Text: "hello"}, 1},
		{"empty text", Result{Text: "  "}, 0},
		{"dispatch error", Result{Text: "partial", Err: errors.New("model down")}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &markingAgent{stubAgent: stubAgent{processResult: tc.result}}
			rcv := &ChannelReceiver{Channel: &stubChannel{name: "web-chat"}, Agent: agent}
			_ = rcv.Receive(context.Background(), conversation.ChannelMessage{
				Source: "web-chat", SessionID: "s", SpeakerID: "u", Text: "hi",
			})
			if agent.marks != tc.want {
				t.Fatalf("marks = %d; want %d", agent.marks, tc.want)
			}
		})
	}
}

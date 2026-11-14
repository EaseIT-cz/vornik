package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/webchat"
)

type webChatSinkStub struct{ key, line string }

func (s *webChatSinkStub) SetWebChatTurnStatus(key, line string) { s.key, s.line = key, line }

type countingResolver struct{ calls int }

func (r *countingResolver) ResolveChannel(string) conversation.Channel { r.calls++; return nil }

// Web chat's status lines go to the UI's sink, never through the channel
// resolver (web-chat-design.md §2.3): the resolver's other callers keep
// resolving web chat to nothing.
func TestTurnStatusReporter_WebChatBranch_WebChat(t *testing.T) {
	res := &countingResolver{}
	r := &turnStatusReporter{resolver: res}
	r.ReportTurnStatus(context.Background(), webchat.ChannelName, "operator:c1", "before the UI exists")
	sink := &webChatSinkStub{}
	r.setWebChatSink(sink)
	r.ReportTurnStatus(context.Background(), webchat.ChannelName, "operator:c1", "⏳ searching memory")
	if sink.key != "operator:c1" || sink.line != "⏳ searching memory" {
		t.Errorf("sink got %q %q", sink.key, sink.line)
	}
	if res.calls != 0 {
		t.Errorf("the web chat branch called the resolver %d times", res.calls)
	}
	r.ReportTurnStatus(context.Background(), "slack", "s1", "x")
	if res.calls != 1 {
		t.Error("other channels still resolve")
	}
}

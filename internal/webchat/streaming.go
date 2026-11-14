package webchat

import (
	"context"

	"vornik.io/vornik/internal/conversation"
)

// StreamingChannel is the web chat channel that streams its reply
// (web-chat-design.md §3.2): each delta the dispatcher writes goes to the
// sink, which the UI's turn registry keeps for the page to poll. It keeps no
// text of its own; the registry bounds what it keeps. The one-shot Channel
// (New) is still what the no-JS page uses: nobody polls that turn.
type StreamingChannel struct {
	*Channel
	sink func(delta string)
}

// NewStreaming returns a web chat channel that streams into sink.
func NewStreaming(projectID string, speaker conversation.Speaker, sink func(delta string)) *StreamingChannel {
	return &StreamingChannel{Channel: New(projectID, speaker), sink: sink}
}

// StreamingSend opens the stream for this turn.
func (c *StreamingChannel) StreamingSend(context.Context, string) (conversation.Stream, error) {
	return webChatStream{sink: c.sink}, nil
}

type webChatStream struct{ sink func(string) }

// Append hands one delta to the sink. It never fails: the sink is in memory.
func (s webChatStream) Append(delta string) error {
	if s.sink != nil && delta != "" {
		s.sink(delta)
	}
	return nil
}

// Close ends the stream. The page's final view comes from the stored
// history, so there is nothing to flush.
func (webChatStream) Close() (string, error) { return "web-chat-stream", nil }

// Compile-time guard.
var _ conversation.StreamingChannel = (*StreamingChannel)(nil)

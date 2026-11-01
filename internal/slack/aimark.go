package slack

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/aimark"
)

// aiMarkHolder holds the EU AI Act Art 50(2) marker (GitHub #117; Art 50
// design §14.8 B, row 15).
type aiMarkHolder struct {
	mu     sync.Mutex
	marker *aimark.Marker
}

// SetAIMarker sets the marker; nil marks nothing.
func (c *Channel) SetAIMarker(m *aimark.Marker) {
	c.aiMark.mu.Lock()
	defer c.aiMark.mu.Unlock()
	c.aiMark.marker = m
}

// recordAIAudio writes the ledger row for a voice reply Slack accepted (row
// 15, opt-in): the clip's bytes as uploaded. A file upload carries no message
// metadata, so the ledger row is the mark.
func (c *Channel) recordAIAudio(ctx context.Context, audio []byte) {
	c.aiMark.mu.Lock()
	m := c.aiMark.marker
	c.aiMark.mu.Unlock()
	if m == nil || len(audio) == 0 {
		return
	}
	_, _, _ = m.New(ctx, audio, "slack_voice", "")
}

// messageMetadata is Slack's message metadata, the mark's carrier: an
// event_type and a payload Slack stores with the message and returns to API
// readers.
// A compact mark is about 300 bytes (a ~150-byte JSON payload and a 64-byte
// Ed25519 signature, base64url), well inside Slack's metadata size limit.
type messageMetadata struct {
	EventType    string            `json:"event_type"`
	EventPayload map[string]string `json:"event_payload"`
}

// markText signs and records the text of an outgoing message and returns its
// metadata, or nil without a marker. A failed mark never withholds the
// message.
func (c *Channel) markText(ctx context.Context, text string) *messageMetadata {
	c.aiMark.mu.Lock()
	m := c.aiMark.marker
	c.aiMark.mu.Unlock()
	if m == nil {
		return nil
	}
	compact, _, err := m.New(ctx, []byte(text), "slack", "")
	if compact == "" {
		c.logger.Warn().Err(err).Msg("slack: AI output mark not created; message sent unmarked")
		return nil
	}
	return &messageMetadata{EventType: "vornik_ai_mark", EventPayload: map[string]string{"mark": compact}}
}

package email

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/aimark"
)

// SetAIMarker sets the EU AI Act Art 50(2) marker (GitHub #117; Art 50
// design §14.8 B, rows 8-10). Nil marks nothing.
func (c *Channel) SetAIMarker(m *aimark.Marker) {
	c.aiMarkerMu.Lock()
	defer c.aiMarkerMu.Unlock()
	c.aiMarker = m
}

// markBody signs and records an outbound body, returning the compact mark
// for the X-AI-Generated header, or "" when there is no marker or no text.
// The mark is over the body as composed (before CRLF normalisation for the
// wire). Marking never withholds the mail.
func (c *Channel) markBody(ctx context.Context, body string) string {
	c.aiMarkerMu.Lock()
	m := c.aiMarker
	c.aiMarkerMu.Unlock()
	if m == nil || strings.TrimSpace(body) == "" {
		return ""
	}
	compact, _, err := m.New(ctx, []byte(body), "email", "")
	if compact == "" {
		c.logger.Warn().Err(err).Msg("email: AI output mark not created; mail sent unmarked")
	}
	return compact
}

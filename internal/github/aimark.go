package github

import (
	"context"
	"strings"
	"sync"

	"vornik.io/vornik/internal/aimark"
)

// aiMarkHolder holds the EU AI Act Art 50(2) marker for the GitHub
// conversation channel's replies (GitHub #117; Art 50 design §14.8).
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

// withMark appends the hidden signed mark to a reply, signed over the text as
// given. A blank text, no marker or a failed mark returns the text unchanged:
// the reply is never withheld.
func (c *Channel) withMark(ctx context.Context, text string) string {
	c.aiMark.mu.Lock()
	m := c.aiMark.marker
	c.aiMark.mu.Unlock()
	if m == nil || strings.TrimSpace(text) == "" {
		return text
	}
	compact, _, _ := m.New(ctx, []byte(text), "github", "")
	if compact == "" {
		return text
	}
	return text + aimark.HiddenComment(compact)
}

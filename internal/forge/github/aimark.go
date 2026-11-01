package github

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/aimark"
)

// SetAIMarker sets the EU AI Act Art 50(2) marker (GitHub #117; Art 50 design
// §14.8 B, rows 1, 2, 4, 7). Nil marks nothing.
func (p *Provider) SetAIMarker(m *aimark.Marker) {
	p.aiMarkerMu.Lock()
	defer p.aiMarkerMu.Unlock()
	p.aiMarker = m
}

// withMark appends the hidden signed mark to a body Vornik posts:
// "\n\n<!-- vornik-ai-mark: <compact> -->", the mark over body as given (a
// verifier strips that line, and the blank line before it, and hashes the
// rest). A blank body, or no marker, is returned unchanged; a failed mark
// never withholds the post.
func (p *Provider) withMark(ctx context.Context, body string) string {
	p.aiMarkerMu.Lock()
	m := p.aiMarker
	p.aiMarkerMu.Unlock()
	if m == nil || strings.TrimSpace(body) == "" {
		return body
	}
	compact, _, _ := m.New(ctx, []byte(body), "github", "")
	if compact == "" {
		return body
	}
	return body + aimark.HiddenComment(compact)
}

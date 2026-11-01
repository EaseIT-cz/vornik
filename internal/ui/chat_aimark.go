package ui

import (
	"context"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// WithAIMarker sets the EU AI Act Art 50(2) marker (GitHub #117); without
// one, nothing is marked.
func WithAIMarker(m *aimark.Marker) ServerOption {
	return func(s *Server) { s.aiMarker = m }
}

// markChatReplies marks each message the dispatcher sent this web chat turn
// (row 16, Art 50 design §14.8 B): one signed mark and ledger row per
// message, over its text as sent. The Art 50(1) disclosure notice is a fixed
// statement, not generated content, and is not marked. It returns the mark
// for each history entry that shows a marked message (matched from the end,
// by content), and puts the turn's marks on the response headers. Marking
// never withholds the reply.
func (s *Server) markChatReplies(ctx context.Context, w http.ResponseWriter, sent []string, history []chat.Message) map[int]string {
	if s.aiMarker == nil {
		return nil
	}
	notice := ""
	if s.aiDisclosure != nil {
		notice = s.aiDisclosure.Notice().Text
	}
	marks := map[int]string{}
	var headerMarks []string
	for _, text := range sent {
		if text == notice || strings.TrimSpace(text) == "" {
			continue
		}
		compact, _, err := s.aiMarker.New(ctx, []byte(text), "web_chat", "")
		if compact == "" {
			s.logger.Warn().Err(err).Msg("web chat: AI output mark not created; reply sent unmarked")
			continue
		}
		headerMarks = append(headerMarks, compact)
		for j := len(history) - 1; j >= 0; j-- {
			if _, taken := marks[j]; !taken && history[j].Role == "assistant" && history[j].Content == text {
				marks[j] = compact
				break
			}
		}
	}
	if len(headerMarks) > 0 {
		w.Header().Set("AI-Disclosure", "ai-generated")
		w.Header().Set("X-Vornik-AI-Mark", strings.Join(headerMarks, ", "))
	}
	return marks
}

// markDownload marks a download of an agent's own output (rows 22, 23, Art
// 50 design §14.8 B) from the artifact's stored SHA-256, so the file is not
// read twice. Uploads, scrapes and rows of unknown origin are not marked:
// nothing says they were generated. Marking never withholds the file.
func (s *Server) markDownload(ctx context.Context, w http.ResponseWriter, a *persistence.Artifact) {
	if s.aiMarker == nil || a == nil || a.Origin != persistence.ArtifactOriginTaskOutput || a.ContentHashSHA256 == nil {
		return
	}
	compact, _, err := s.aiMarker.NewForHash(ctx, *a.ContentHashSHA256, "download", "")
	if compact == "" {
		s.logger.Warn().Err(err).Str("artifact_id", a.ID).Msg("download: AI output mark not created; file sent unmarked")
		return
	}
	w.Header().Set("AI-Disclosure", "ai-generated")
	w.Header().Set("X-Vornik-AI-Mark", compact)
}

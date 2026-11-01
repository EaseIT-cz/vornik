package api

import (
	"context"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskkeys"
)

// EU AI Act Art 50(2) machine-readable marking of the text the API returns
// (Art 50 design §14.8 B, GitHub #117).

// WithAIMarker sets the marker that signs and records the marks; without
// one, nothing is marked.
func WithAIMarker(m *aimark.Marker) ServerOption {
	return func(s *Server) { s.aiMarker = m }
}

// executorAgentKey reports whether r authenticated with a key the executor
// minted for an agent container. An agent's own model calls are
// machine-to-machine (Commission guidelines on Art 50, para 68) and are not
// marked. The test is on the credential alone, never a request header: a
// header such as X-Vornik-Task-ID is client-settable and would let any
// caller opt out. No grant or rotation path writes these created_by values
// (idle-gate design §8.5a); do not widen this without the Art 50 design.
func executorAgentKey(r *http.Request) bool {
	id := IdentityFromContext(r.Context())
	if id == nil || id.Extra == nil {
		return false
	}
	row, _ := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	if row == nil || row.ClientKind != "" {
		return false
	}
	switch row.CreatedBy {
	case executorKeyCreator:
		return strings.HasPrefix(row.Name, persistence.TaskKeyNamePrefix)
	case executorWarmKeyCreator:
		return strings.HasPrefix(row.Name, taskkeys.WarmAgentKeyNamePrefix)
	}
	return false
}

// executorWarmKeyCreator is the created_by of a warm-pool agent key
// (taskkeys.MintProjectScopedKey).
const executorWarmKeyCreator = "executor-warm"

// markOutput signs and records text sent on surface and returns the compact
// mark, or "" when nothing is to be marked (no marker, or an agent's own
// call). A ledger failure still returns the mark: the output is never
// withheld (§14.7).
func (s *Server) markOutput(ctx context.Context, r *http.Request, text, surface string) string {
	if s.aiMarker == nil || executorAgentKey(r) {
		return ""
	}
	compact, _, err := s.aiMarker.New(ctx, []byte(text), surface, "")
	if err != nil && compact == "" {
		s.logger.Warn().Err(err).Str("surface", surface).Msg("AI output mark not created; output sent unmarked")
	}
	return compact
}

// setMarkHeaders puts the mark on a response.
func setMarkHeaders(w http.ResponseWriter, compact string) {
	w.Header().Set("AI-Disclosure", "ai-generated")
	w.Header().Set("X-Vornik-AI-Mark", compact)
}

// chatResponseText is the text a chat response returns: every choice's
// message content, in order, joined by newlines. The mark is over these
// bytes.
func chatResponseText(resp *chat.ChatResponse) string {
	parts := make([]string, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		parts = append(parts, c.Message.Content)
	}
	return strings.Join(parts, "\n")
}

// ollamaMark marks Ollama-compatible output (row 19). headers is false on a
// stream, whose headers are already sent: the final line carries the mark.
func (s *Server) ollamaMark(w http.ResponseWriter, r *http.Request, text string, headers bool) *aiGenerated {
	compact := s.markOutput(r.Context(), r, text, "ollama_api")
	if compact == "" {
		return nil
	}
	if headers {
		setMarkHeaders(w, compact)
	}
	return &aiGenerated{Mark: compact}
}

// aiGenerated is the JSON carrier of the mark on an API body.
type aiGenerated struct {
	Mark string `json:"mark"`
}

// markArtifactDownload marks a download of an agent's own output (row 22,
// Art 50 design §14.8 B) from the artifact's stored SHA-256, which the
// download path has verified against the bytes it serves. Uploads, scrapes
// and rows of unknown origin are not marked: nothing says they were
// generated. Marking never withholds the file.
func (s *Server) markArtifactDownload(ctx context.Context, w http.ResponseWriter, a *persistence.Artifact) {
	if s.aiMarker == nil || a == nil || a.Origin != persistence.ArtifactOriginTaskOutput || a.ContentHashSHA256 == nil {
		return
	}
	compact, _, err := s.aiMarker.NewForHash(ctx, *a.ContentHashSHA256, "download", "")
	if compact == "" {
		s.logger.Warn().Err(err).Str("artifact_id", a.ID).Msg("download: AI output mark not created; file sent unmarked")
		return
	}
	setMarkHeaders(w, compact)
}

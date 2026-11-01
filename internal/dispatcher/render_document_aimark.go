package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"

	"vornik.io/vornik/internal/aimark"
)

// aiMarkHolder holds the EU AI Act Art 50(2) marker for render_document
// (GitHub #117; Art 50 design §14.8, commit 5, row 27).
type aiMarkHolder struct {
	mu     sync.Mutex
	marker *aimark.Marker
}

// SetAIMarker sets the marker; nil leaves documents with the unsigned
// statement only.
func (te *ToolExecutor) SetAIMarker(m *aimark.Marker) {
	te.aiMark.mu.Lock()
	defer te.aiMark.mu.Unlock()
	te.aiMark.marker = m
}

func (te *ToolExecutor) markerOrNil() *aimark.Marker {
	te.aiMark.mu.Lock()
	defer te.aiMark.mu.Unlock()
	return te.aiMark.marker
}

// markSource signs the markdown source a document is rendered from and
// returns the compact mark, or "" without a marker. A failure never stops
// the render.
func (te *ToolExecutor) markSource(ctx context.Context, src string) string {
	m := te.markerOrNil()
	if m == nil {
		return ""
	}
	compact, _, _ := m.New(ctx, []byte(src), "document", "")
	return compact
}

// recordRendered writes a ledger row for each rendered file, so an exact copy
// found later is found by its hash (the embedded mark covers the source).
// The markdown source, already recorded by markSource, is skipped.
func (te *ToolExecutor) recordRendered(ctx context.Context, paths []string, mdPath string) {
	m := te.markerOrNil()
	if m == nil {
		return
	}
	for _, p := range paths {
		if p == mdPath {
			continue
		}
		b, err := os.ReadFile(p) //nolint:gosec // the render's own temp output
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		_, _, _ = m.NewForHash(ctx, hex.EncodeToString(sum[:]), "document", "")
	}
}

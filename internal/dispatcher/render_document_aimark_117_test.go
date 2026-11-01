package dispatcher

// GitHub #117, Art 50(2) step 2 commit 5, row 27 (Art 50 design §14.8): a
// document render_document produces carries a signed mark over its markdown
// source in its metadata, and the ledger records the source and each
// rendered file by hash.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/docrender"
	"vornik.io/vornik/internal/persistence"
)

type renderMarkLedger struct {
	mu     sync.Mutex
	hashes []string
}

func (l *renderMarkLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hashes = append(l.hashes, m.ContentSHA256)
	return nil
}
func (l *renderMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func hexSum(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }

func TestRenderDocument117_MetadataAndLedger(t *testing.T) {
	sb := &fakeSandbox{}
	te := sandboxExecutor(t, sb)
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &renderMarkLedger{}
	te.SetAIMarker(aimark.NewMarker(signer, ledger, func() string { return "t" }, nil, nil))
	src := "# Report\n\nGenerated text."
	args, _ := json.Marshal(map[string]any{"content": src, "name": "report", "formats": []string{"html", "pdf"}})
	res := te.renderDocument(context.Background(), string(args), &stubSenderRecording{})
	if !strings.Contains(res.Content, "Delivered") {
		t.Fatalf("got %q", res.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(sb.meta), &meta); err != nil {
		t.Fatalf("meta %q: %v", sb.meta, err)
	}
	compact, _ := meta["vornik-ai-mark"].(string)
	m, err := aimark.Verify(compact, signer.Keys())
	if err != nil {
		t.Fatalf("meta mark does not verify: %v (%v)", err, meta)
	}
	if m.SHA256 != hexSum(src) || m.Surface != "document" {
		t.Errorf("mark %+v: want the hash of the markdown source", m)
	}
	if d, _ := meta["description"].(string); !strings.Contains(d, "vornik-ai-mark: "+compact) || meta["title"] != "report" {
		t.Errorf("meta %v", meta)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var src1, out int
	for _, h := range ledger.hashes {
		switch h {
		case hexSum(src):
			src1++
		case hexSum("rendered"): // the fake sandbox's output bytes
			out++
		}
	}
	if src1 != 1 || out != 2 {
		t.Errorf("ledger rows: source %d (want 1), rendered files %d (want 2): %v", src1, out, ledger.hashes)
	}
}

// Without a signer the document still says what it is, unsigned.
func TestRenderDocument117_NoMarkerStatesItUnsigned(t *testing.T) {
	sb := &fakeSandbox{}
	te := sandboxExecutor(t, sb)
	te.renderDocument(context.Background(), `{"content":"# x","name":"r","formats":["html"]}`, &stubSenderRecording{})
	var meta map[string]any
	_ = json.Unmarshal([]byte(sb.meta), &meta)
	if _, ok := meta["vornik-ai-mark"]; ok || meta["title"] != "r" || meta["description"] != docrender.AIGeneratedDescription {
		t.Errorf("meta %v", meta)
	}
}

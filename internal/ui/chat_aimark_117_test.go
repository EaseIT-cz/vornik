package ui

// GitHub #117, Art 50(2) step 2 commit 1b, row 16 (Art 50 design §14.8 B):
// a web chat reply carries its signed mark, on its bubble and in a header.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type uiMarkLedger struct {
	mu   sync.Mutex
	rows []persistence.AIOutputMark
}

func (l *uiMarkLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, m)
	return nil
}
func (l *uiMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func uiTestMarker() (*aimark.Marker, *aimark.Signer, *uiMarkLedger) {
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &uiMarkLedger{}
	return aimark.NewMarker(signer, ledger, func() string { return "test" }, nil, nil), signer, ledger
}

var dataMarkRE = regexp.MustCompile(`data-ai-mark="([^"]+)"`)

func TestWebChat117_ReplyCarriesItsMark(t *testing.T) {
	server, _ := chatTestServer(t, "hello from the agent")
	marker, signer, ledger := uiTestMarker()
	server.aiMarker = marker
	form := url.Values{"prompt": {"what's up?"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req = asOperator491(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "test-session"})
	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	ms := dataMarkRE.FindAllStringSubmatch(rec.Body.String(), -1)
	if len(ms) != 1 {
		t.Fatalf("data-ai-mark attributes %d, want 1 (the reply's bubble only)", len(ms))
	}
	m, err := aimark.Verify(ms[0][1], signer.Keys())
	if err != nil {
		t.Fatalf("the bubble's mark does not verify: %v", err)
	}
	sum := sha256.Sum256([]byte("hello from the agent"))
	if m.SHA256 != hex.EncodeToString(sum[:]) || m.Surface != "web_chat" {
		t.Errorf("mark %+v: want the reply's hash on web_chat", m)
	}
	if rec.Header().Get("X-Vornik-AI-Mark") != ms[0][1] || rec.Header().Get("AI-Disclosure") != "ai-generated" {
		t.Errorf("headers %q / %q", rec.Header().Get("X-Vornik-AI-Mark"), rec.Header().Get("AI-Disclosure"))
	}
	if len(ledger.rows) != 1 {
		t.Errorf("ledger rows %d, want 1 (the disclosure notice is not marked)", len(ledger.rows))
	}
}

func TestWebChat117_NoMarkerNoMark(t *testing.T) {
	server, _ := chatTestServer(t, "hello from the agent")
	form := url.Values{"prompt": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req = asOperator491(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "p1")
	if strings.Contains(rec.Body.String(), "data-ai-mark") || rec.Header().Get("X-Vornik-AI-Mark") != "" {
		t.Error("no marker must mean no mark")
	}
}

// Rows 22, 23 (commit 1b): a download of an agent's own output carries its
// mark, from the stored hash; uploads, scrapes and legacy rows do not, since
// nothing says they were generated.
func TestArtifactDownload117_MarksTaskOutputOnly(t *testing.T) {
	body := []byte("from-backend")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		origin persistence.ArtifactOrigin
		marked bool
	}{
		{persistence.ArtifactOriginTaskOutput, true},
		{persistence.ArtifactOriginUpload, false},
		{persistence.ArtifactOriginWebScrape, false},
		{persistence.ArtifactOriginUnknown, false},
	} {
		repo := &mocks.MockArtifactRepository{GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{ID: "a1", Name: "f.txt", StoragePath: "/nope", Origin: c.origin, ContentHashSHA256: &hash}, nil
		}}
		marker, signer, ledger := uiTestMarker()
		srv := NewServer(WithArtifactRepository(repo), WithArtifactReader(&stubArtifactReader{body: body}), WithAIMarker(marker))
		rec := httptest.NewRecorder()
		srv.ArtifactDownload(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil))
		compact := rec.Header().Get("X-Vornik-AI-Mark")
		if !c.marked {
			if compact != "" || len(ledger.rows) != 0 {
				t.Errorf("%s: marked", c.origin)
			}
			continue
		}
		m, err := aimark.Verify(compact, signer.Keys())
		if err != nil || m.SHA256 != hash || m.Surface != "download" || rec.Body.String() != "from-backend" {
			t.Errorf("%s: mark %+v err %v body %q", c.origin, m, err, rec.Body.String())
		}
	}
}

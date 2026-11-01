package api

// GitHub #117, Art 50(2) step 2 commit 1 (Art 50 design §14.8 B): the
// OpenAI-compatible API marks the text it returns, except to an agent
// container calling with its executor-minted key (machine to machine,
// guidelines para 68). Found at: design (the law's deadline); a caller
// observes the mark on the HTTP response, so the handler is exercised whole.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

type memMarkLedger struct {
	mu   sync.Mutex
	rows []persistence.AIOutputMark
}

func (l *memMarkLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, m)
	return nil
}
func (l *memMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }
func (l *memMarkLedger) count() int                                             { l.mu.Lock(); defer l.mu.Unlock(); return len(l.rows) }

func testMarker() (*aimark.Marker, *aimark.Signer, *memMarkLedger) {
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &memMarkLedger{}
	return aimark.NewMarker(signer, ledger, func() string { return "test" }, nil, nil), signer, ledger
}

func postChat117(t *testing.T, s *Server, key *persistence.APIKey, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if key != nil {
		req = req.WithContext(context.WithValue(req.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: key}}))
	}
	rec := httptest.NewRecorder()
	s.ChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestChatAPI117_MarksTheReplyText(t *testing.T) {
	marker, signer, ledger := testMarker()
	s := NewServer(WithChatProvider(stubChatProviderWithUsage{model: "m", respModel: "m"}), WithAIMarker(marker))
	rec := postChat117(t, s, nil, nil)

	if got := rec.Header().Get("AI-Disclosure"); got != "ai-generated" {
		t.Errorf("AI-Disclosure = %q", got)
	}
	compact := rec.Header().Get("X-Vornik-AI-Mark")
	m, err := aimark.Verify(compact, signer.Keys())
	if err != nil {
		t.Fatalf("the header mark does not verify: %v (%q)", err, compact)
	}
	sum := sha256.Sum256([]byte("ok"))
	if m.SHA256 != hex.EncodeToString(sum[:]) || m.Surface != "chat_api" {
		t.Errorf("mark %+v: want the hash of the reply text and surface chat_api", m)
	}
	var body struct {
		AIGenerated struct {
			Mark string `json:"mark"`
		} `json:"ai_generated"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AIGenerated.Mark != compact || len(body.Choices) != 1 {
		t.Errorf("body ai_generated.mark %q, choices %d: want the header's mark and the reply unchanged", body.AIGenerated.Mark, len(body.Choices))
	}
	if ledger.count() != 1 {
		t.Errorf("ledger rows %d, want 1", ledger.count())
	}
}

func TestChatAPI117_AgentKeyIsNotMarked(t *testing.T) {
	marker, _, ledger := testMarker()
	s := NewServer(WithChatProvider(stubChatProviderWithUsage{model: "m", respModel: "m"}), WithAIMarker(marker))
	for _, key := range []*persistence.APIKey{
		{Name: persistence.TaskKeyNamePrefix + "t1", CreatedBy: "executor"},
		{Name: "agent:warm_p:coder", CreatedBy: "executor-warm"},
	} {
		rec := postChat117(t, s, key, nil)
		if rec.Header().Get("X-Vornik-AI-Mark") != "" || strings.Contains(rec.Body.String(), "ai_generated") {
			t.Errorf("%s: an agent's own model call was marked", key.Name)
		}
	}
	if ledger.count() != 0 {
		t.Errorf("ledger rows %d, want 0 for agent calls", ledger.count())
	}
}

func TestChatAPI117_HeadersAndLookalikeKeysCannotOptOut(t *testing.T) {
	marker, _, ledger := testMarker()
	s := NewServer(WithChatProvider(stubChatProviderWithUsage{model: "m", respModel: "m"}), WithAIMarker(marker))
	cases := []struct {
		name   string
		key    *persistence.APIKey
		header map[string]string
	}{
		{"task header, no key", nil, map[string]string{"X-Vornik-Task-ID": "task_1", "X-Vornik-Execution-ID": "exec_1"}},
		{"task-prefixed name, operator-created", &persistence.APIKey{Name: persistence.TaskKeyNamePrefix + "t1", CreatedBy: "akey_op"}, nil},
		{"executor-created, client kind set", &persistence.APIKey{Name: persistence.TaskKeyNamePrefix + "t1", CreatedBy: "executor", ClientKind: "claude-code"}, nil},
		{"executor-created, other name", &persistence.APIKey{Name: "my-key", CreatedBy: "executor"}, nil},
	}
	for _, c := range cases {
		if rec := postChat117(t, s, c.key, c.header); rec.Header().Get("X-Vornik-AI-Mark") == "" {
			t.Errorf("%s: not marked", c.name)
		}
	}
	if ledger.count() != len(cases) {
		t.Errorf("ledger rows %d, want %d", ledger.count(), len(cases))
	}
}

func TestChatAPI117_NoMarkerStillAnswers(t *testing.T) {
	s := NewServer(WithChatProvider(stubChatProviderWithUsage{model: "m", respModel: "m"}))
	rec := postChat117(t, s, nil, nil)
	if rec.Header().Get("X-Vornik-AI-Mark") != "" || strings.Contains(rec.Body.String(), "ai_generated") {
		t.Error("no marker must mean no mark")
	}
}

// Row 22 (commit 1b): the companion's binary download marks an agent's own
// output from the stored, verified hash; other origins are not marked.
func TestCompanionDownload117_MarksTaskOutputOnly(t *testing.T) {
	sum := sha256.Sum256([]byte("bytes"))
	hash := hex.EncodeToString(sum[:])
	for _, origin := range []persistence.ArtifactOrigin{persistence.ArtifactOriginTaskOutput, persistence.ArtifactOriginUpload, persistence.ArtifactOriginWebScrape, persistence.ArtifactOriginUnknown} {
		marker, signer, ledger := testMarker()
		s := NewServer(WithAIMarker(marker))
		rec := httptest.NewRecorder()
		s.markArtifactDownload(context.Background(), rec, &persistence.Artifact{ID: "a", Origin: origin, ContentHashSHA256: &hash})
		compact := rec.Header().Get("X-Vornik-AI-Mark")
		if origin != persistence.ArtifactOriginTaskOutput {
			if compact != "" || ledger.count() != 0 {
				t.Errorf("%s: marked", origin)
			}
			continue
		}
		if m, err := aimark.Verify(compact, signer.Keys()); err != nil || m.SHA256 != hash || m.Surface != "download" {
			t.Errorf("task_output: mark %+v err %v", m, err)
		}
	}
}

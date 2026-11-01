package api

// GitHub #117, Art 50(2) step 2 commit 1, row 19 (Art 50 design §14.8 B): the
// Ollama-compatible API marks the text it returns. A stream carries the mark
// on its final done:true line only, over the text of the deltas it sent.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/aimark"
)

func ollamaLines(t *testing.T, body *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func checkMark(t *testing.T, compact, text, surface string, signer *aimark.Signer) {
	t.Helper()
	m, err := aimark.Verify(compact, signer.Keys())
	if err != nil {
		t.Fatalf("mark does not verify: %v (%q)", err, compact)
	}
	sum := sha256.Sum256([]byte(text))
	if m.SHA256 != hex.EncodeToString(sum[:]) || m.Surface != surface {
		t.Errorf("mark %+v: want the hash of %q on %s", m, text, surface)
	}
}

func TestOllama117_StreamMarksOnlyTheFinalLine(t *testing.T) {
	for _, path := range []string{"/api/chat", "/api/generate"} {
		marker, signer, ledger := testMarker()
		stub := &streamingStub{model: "m", chunks: []string{"Hello", " there", "!"}, finalResp: buildOllamaOKResponse("m", "Hello there!")}
		s := NewServer(WithChatProvider(stub), WithAIMarker(marker))
		body := `{"messages":[{"role":"user","content":"hi"}]}`
		if path == "/api/generate" {
			body = `{"prompt":"hi"}`
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		if path == "/api/chat" {
			s.OllamaChat(rr, req)
		} else {
			s.OllamaGenerate(rr, req)
		}
		lines := ollamaLines(t, rr.Body)
		if len(lines) < 2 {
			t.Fatalf("%s: %d lines", path, len(lines))
		}
		for _, l := range lines[:len(lines)-1] {
			if _, ok := l["ai_generated"]; ok {
				t.Errorf("%s: a line before the last carries the mark: %v", path, l)
			}
		}
		last := lines[len(lines)-1]
		ag, _ := last["ai_generated"].(map[string]any)
		compact, _ := ag["mark"].(string)
		if last["done"] != true || compact == "" {
			t.Fatalf("%s: final line %v: want done:true with ai_generated.mark", path, last)
		}
		checkMark(t, compact, "Hello there!", "ollama_api", signer)
		if ledger.count() != 1 {
			t.Errorf("%s: ledger rows %d, want 1", path, ledger.count())
		}
	}
}

func TestOllama117_NonStreamMarksBodyAndHeader(t *testing.T) {
	for _, path := range []string{"/api/chat", "/api/generate"} {
		marker, signer, _ := testMarker()
		stub := &streamingStub{model: "m", finalResp: buildOllamaOKResponse("m", "Hello there!")}
		s := NewServer(WithChatProvider(stub), WithAIMarker(marker))
		body := `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`
		if path == "/api/generate" {
			body = `{"stream":false,"prompt":"hi"}`
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		if path == "/api/chat" {
			s.OllamaChat(rr, req)
		} else {
			s.OllamaGenerate(rr, req)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v: %s", path, err, rr.Body.String())
		}
		ag, _ := out["ai_generated"].(map[string]any)
		compact, _ := ag["mark"].(string)
		if compact == "" || rr.Header().Get("X-Vornik-AI-Mark") != compact {
			t.Fatalf("%s: body mark %q, header %q", path, compact, rr.Header().Get("X-Vornik-AI-Mark"))
		}
		checkMark(t, compact, "Hello there!", "ollama_api", signer)
	}
}

// A stream that fails mid-way has no complete output: its error line is not
// marked and nothing is recorded (review c87d).
func TestOllama117_ErrorStreamIsNotMarked(t *testing.T) {
	marker, _, ledger := testMarker()
	s := NewServer(WithChatProvider(&streamingStub{model: "m", err: errorString("upstream down")}), WithAIMarker(marker))
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)))
	for _, l := range ollamaLines(t, rr.Body) {
		if _, ok := l["ai_generated"]; ok {
			t.Errorf("an error line carries a mark: %v", l)
		}
	}
	if ledger.count() != 0 {
		t.Errorf("ledger rows %d, want 0", ledger.count())
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

package api

// GitHub #425 (residual of #414, chat-audit retention and redaction design
// §11.2.3): both cost-attribution warners logged the raw User-Agent — any
// length, control bytes and invalid UTF-8 included — and keyed their rate
// limit on it. Found at: review (#414's §10.2.5). A user observes it in the
// daemon log, so the test drives the real warners into a real zerolog sink
// and reads the line back.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// hostileUA425 carries a control byte (a log-line injection attempt), invalid
// UTF-8 and is well over the identity bound.
var hostileUA425 = "curl/8.4\n{\"level\":\"error\"} \xff\xfe " + strings.Repeat("ab cd ", 50)

func requireBoundedUA425(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("one warn line expected, got %d: %q", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("warn line is not JSON: %v\n%s", err, lines[0])
	}
	ua, _ := entry["user_agent"].(string)
	if ua == "" || len(ua) > 128 {
		t.Fatalf("user_agent must be the bounded form (1..128 bytes), got %d bytes: %q", len(ua), ua)
	}
	if !utf8.ValidString(ua) || strings.ContainsAny(ua, "\n\r\x00") || strings.ContainsRune(ua, utf8.RuneError) {
		t.Fatalf("user_agent must be valid UTF-8 without control bytes: %q", ua)
	}
	if !strings.HasPrefix(ua, "curl/8.4") {
		t.Fatalf("the bound keeps the identity's head: %q", ua)
	}
}

func TestWarnOnAnonymousAttribution425_LogsBoundedUserAgent(t *testing.T) {
	srv, buf := newWarnTestServer()
	r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	r.Header["User-Agent"] = []string{hostileUA425}
	srv.warnOnAnonymousAttribution(r, AttributionAnonymous)
	requireBoundedUA425(t, buf)
}

func TestMaybeWarnLegacyHeaderShadowed425_LogsBoundedUserAgent(t *testing.T) {
	buf := &bytes.Buffer{}
	s := &Server{logger: zerolog.New(buf).Level(zerolog.WarnLevel)}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	r.Header.Set("X-Vornik-Project-ID", "header-project")
	r.Header["User-Agent"] = []string{hostileUA425}
	r = r.WithContext(context.WithValue(r.Context(), projectIDFromKeyKey, "key-project"))
	s.maybeWarnLegacyHeaderShadowed(httptest.NewRecorder(), r)
	requireBoundedUA425(t, buf)
}

// The rate-limit key is the bounded value too: two headers that bound to the
// same identity are one client, not an unbounded set of map keys.
func TestWarnOnAnonymousAttribution425_RateLimitKeyIsBounded(t *testing.T) {
	srv, buf := newWarnTestServer()
	for _, tail := range []string{"x", "y"} {
		r := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		r.Header["User-Agent"] = []string{"client/1 " + strings.Repeat("ab cd ", 40) + tail}
		srv.warnOnAnonymousAttribution(r, AttributionAnonymous)
	}
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; n != 1 {
		t.Fatalf("two headers with the same bounded form must share one rate-limit slot; got %d lines", n)
	}
	srv.anonAttrWarner.mu.Lock()
	defer srv.anonAttrWarner.mu.Unlock()
	for k := range srv.anonAttrWarner.lastFor {
		if len(k) > len("/api/chat")+1+128 {
			t.Fatalf("rate-limit key is unbounded: %d bytes", len(k))
		}
	}
}

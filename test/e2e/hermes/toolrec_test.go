package hermes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamEcho answers every request with the body it received, so a test
// can see what the recorder forwarded.
func upstreamEcho(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postThrough(t *testing.T, h http.Handler, path, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("forwarded body is not JSON: %v: %s", err, rec.Body.String())
	}
	return out
}

// Hermes e2e lane bring-up, 2026-09-30: Hermes's session store does not
// record the tools it offered, so with a real model H1's tool-set check
// never ran. The recorder sees them on the wire.
func TestToolRecorderRecordsOfferedTools(t *testing.T) {
	rec := NewToolRecorder(upstreamEcho(t).URL)
	postThrough(t, rec, "/v1/chat/completions",
		`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"vornik_status"}},{"type":"function","function":{"name":"vornik_catalog"}}]}`)
	got := strings.Join(rec.ToolsSeen(), ",")
	if got != "vornik_catalog,vornik_status" {
		t.Fatalf("ToolsSeen = %q", got)
	}
}

// Hermes e2e lane, 2026-09-30: llama.cpp runs at temperature 0, so H1's
// three attempts were byte-identical and the retry could not recover
// anything. The first attempt is forwarded unchanged; later attempts are
// re-sampled with a per-attempt seed.
func TestToolRecorderResamplesOnRetry(t *testing.T) {
	rec := NewToolRecorder(upstreamEcho(t).URL)
	first := postThrough(t, rec, "/v1/chat/completions", `{"model":"m","messages":[],"temperature":0}`)
	if first["temperature"] != float64(0) || first["seed"] != nil {
		t.Fatalf("attempt 1 was modified: %v", first)
	}
	rec.SetAttempt(3)
	third := postThrough(t, rec, "/v1/chat/completions", `{"model":"m","messages":[],"temperature":0}`)
	if third["temperature"] != retryTemperature || third["seed"] != float64(3) {
		t.Fatalf("attempt 3 not re-sampled: %v", third)
	}
}

func TestToolRecorderPassesOtherPathsThrough(t *testing.T) {
	rec := NewToolRecorder(upstreamEcho(t).URL)
	rec.SetAttempt(2)
	out := postThrough(t, rec, "/v1/embeddings", `{"input":"x"}`)
	if out["input"] != "x" || out["seed"] != nil {
		t.Fatalf("non-completion request changed: %v", out)
	}
	if len(rec.ToolsSeen()) != 0 {
		t.Fatalf("tools recorded from a non-completion request: %v", rec.ToolsSeen())
	}
}

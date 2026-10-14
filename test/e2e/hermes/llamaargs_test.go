package hermes

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// GitHub #380, Hermes release lane H3f-forget (run 4400844a, 2026-10-14):
// llama.cpp ran with no reply bound; one degenerate tool call ran 3,381 and
// 3,488 tokens at about 6.8 tokens/s, and an earlier run hit the package's
// 60-minute timeout in the same step. Lane design, "Amendment 2026-10-14 — a
// bound on one model reply".

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLlamaMaxTokens_DefaultOverrideAndRefusals(t *testing.T) {
	if n, err := llamaMaxTokens(envOf(nil)); err != nil || n != 1024 {
		t.Fatalf("default = %d, %v; want 1024", n, err)
	}
	if n, err := llamaMaxTokens(envOf(map[string]string{"VORNIK_E2E_LLAMA_MAX_TOKENS": "2048"})); err != nil || n != 2048 {
		t.Fatalf("override = %d, %v; want 2048", n, err)
	}
	// An override that is not a positive integer must fail, never remove the
	// bound (llama.cpp reads -1 as "no limit").
	for _, bad := range []string{"0", "-1", "abc", " ", "1e3", "1024 "} {
		if n, err := llamaMaxTokens(envOf(map[string]string{"VORNIK_E2E_LLAMA_MAX_TOKENS": bad})); err == nil {
			t.Errorf("override %q accepted as %d; want an error", bad, n)
		}
	}
}

func TestLlamaServerArgs_BoundReplyLength(t *testing.T) {
	m := laneModels[defaultLaneModel]
	threads, err := llamaThreads(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	args := llamaServerArgs(4242, "/models-dir", "img:tag", m, threads, 1024)
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" -n 1024 ", " -t 6 ", " --temp 0 ", " --seed 1 ", " -c 65536 ", " -np 1 ",
		" --jinja ", " -m /models/" + m.File + " ", " -p 127.0.0.1:4242:8080 ", " -v /models-dir:/models:ro,Z img:tag "} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", strings.TrimSpace(want), joined)
		}
	}
	if !slices.Equal(args[len(args)-len(m.Args):], m.Args) {
		t.Errorf("the model's own arguments are not last: %v", args)
	}
	if got, err := llamaThreads(envOf(map[string]string{"VORNIK_E2E_LLAMA_THREADS": "14"})); err != nil || got != "14" {
		t.Errorf("threads override = %q, %v", got, err)
	}
	for _, bad := range []string{"0", "-2", "abc", "6 "} {
		if got, err := llamaThreads(envOf(map[string]string{"VORNIK_E2E_LLAMA_THREADS": bad})); err == nil {
			t.Errorf("threads %q accepted as %q; want an error", bad, got)
		}
	}
	// No pinned model may carry its own reply bound: it would come after -n
	// and win.
	for name, lm := range laneModels {
		for _, a := range lm.Args {
			if a == "-n" || a == "--n-predict" || a == "--predict" {
				t.Errorf("model %s sets %s in its own arguments", name, a)
			}
		}
	}
}

// At llama.cpp b11610 (091b6a7fc) a request's max_tokens replaces the
// server's -n ("the per-request limit takes priority over the global one",
// tools/server/server-context.cpp), and Hermes retries a truncated tool call
// with a boosted max_tokens (4096 * 2^n). So the proxy Hermes talks to
// enforces the cap on every completion request.
func TestToolRecorderCapsReplyLength(t *testing.T) {
	rec := NewToolRecorder(upstreamEcho(t).URL, 1024)
	cases := []struct{ body, why string }{
		{`{"model":"m","messages":[]}`, "no bound sent"},
		{`{"model":"m","messages":[],"max_tokens":8192}`, "Hermes's first boosted retry"},
		{`{"model":"m","messages":[],"max_completion_tokens":32768}`, "the OpenAI name"},
		{`{"model":"m","messages":[],"n_predict":-1}`, "llama.cpp's unlimited"},
		{`{"model":"m","messages":[],"max_tokens":-1,"n_predict":65536}`, "both, unbounded"},
		{`{"model":"m","messages":[],"max_tokens":"lots"}`, "not a number"},
	}
	for _, c := range cases {
		out := postThrough(t, rec, "/v1/chat/completions", c.body)
		if out["max_tokens"] != float64(1024) || out["n_predict"] != nil || out["max_completion_tokens"] != nil {
			t.Errorf("%s: forwarded %v; want max_tokens 1024 and no other bound", c.why, out)
		}
	}
	out := postThrough(t, rec, "/v1/chat/completions", `{"model":"m","messages":[],"max_tokens":200,"temperature":0}`)
	if out["max_tokens"] != float64(200) || out["temperature"] != float64(0) || out["seed"] != nil {
		t.Errorf("a smaller bound or attempt 1's sampling changed: %v", out)
	}
	out = postThrough(t, rec, "/v1/chat/completions", `{"model":"m","messages":[],"max_tokens":4000,"n_predict":300}`)
	if out["max_tokens"] != float64(300) {
		t.Errorf("the smallest requested bound must win: %v", out)
	}
}

// A body that is not a JSON object is forwarded byte for byte, with no bound
// injected (llama.cpp refuses it; the server's -n is the only bound there).
// Review 454e F5 pins the documented fallback.
func TestToolRecorderForwardsANonObjectBodyUnchanged(t *testing.T) {
	rec := NewToolRecorder(upstreamEcho(t).URL, 1024)
	for _, body := range []string{"null", "[1,2]", "not json"} {
		w := httptest.NewRecorder()
		rec.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if got := w.Body.String(); got != body {
			t.Errorf("forwarded %q as %q", body, got)
		}
	}
}

package agentbench

// GitHub #827 (agent-quality benchmark design, Amendment 2026-11-18 (#827)):
// a llama-server endpoint answered neither the Ollama check nor the refusal
// probe, so every calibration preflight printed the window "unverified".
// Found at: e2e-lane (the gemma4-26b calibration arm). Test at: unit, a fake
// server with the measured /props body. Lower because: the bench lane's
// server is the operator's GPU box.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The reference server's /props, measured 2026-11-18 (trimmed).
const llamaProps1 = `{"default_generation_settings":{"n_ctx":131072},"total_slots":1,"model_alias":"gemma4-26b"}`

// A two-slot server (-c 131072 -np 2): the slot window is 65536.
const llamaProps2 = `{"default_generation_settings":{"n_ctx":65536},"total_slots":2,"model_alias":"gemma4-26b"}`

func llamaServer(t *testing.T, props string, completions *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			if props == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(props))
		case "/api/ps", "/api/version":
			http.NotFound(w, r) // not Ollama
		default:
			completions.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"the request exceeds the available context size"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverModelWindow_LlamaServerReadsProps_827(t *testing.T) {
	for name, c := range map[string]struct {
		props string
		want  int
	}{
		"one slot":  {llamaProps1, 131072},
		"two slots": {llamaProps2, 65536},
	} {
		var completions atomic.Int32
		srv := llamaServer(t, c.props, &completions)
		n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "vllm/gemma4-26b")
		if err != nil || n != c.want {
			t.Errorf("%s: got %d (%v), want %d", name, n, err, c.want)
		}
		if completions.Load() != 0 {
			t.Errorf("%s: an answered /props must not run the refusal probe", name)
		}
	}
}

func TestDiscoverModelWindow_NoPropsFallsThroughToTheRefusalProbe_827(t *testing.T) {
	var completions atomic.Int32
	srv := llamaServer(t, "", &completions)
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err != nil || n != 0 {
		t.Fatalf("got %d (%v), want undiscovered", n, err)
	}
	if completions.Load() != 1 {
		t.Fatalf("the refusal probe ran %d times, want 1", completions.Load())
	}
}

// Review 0449 F1: a /props-shaped body without total_slots is not taken for
// llama-server; the refusal probe still runs.
func TestDiscoverModelWindow_PropsWithoutSlotsIsNotLlamaServer_827(t *testing.T) {
	var completions atomic.Int32
	srv := llamaServer(t, `{"default_generation_settings":{"n_ctx":131072}}`, &completions)
	if n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "m"); err != nil || n != 0 {
		t.Fatalf("got %d (%v), want undiscovered", n, err)
	}
	if completions.Load() != 1 {
		t.Fatalf("the refusal probe ran %d times, want 1", completions.Load())
	}
}

package agentbench

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// An Ollama server is asked for its served window directly (benchmark LLD
// §12.11.3, amended 2026-09-26). Incident: the slow-hardware arms, where the
// refusal probe (an impossible max_tokens) met an Ollama server that clamps and
// generates instead of refusing, ran into its deadline, and left both arms'
// window "unverified".

const ollamaPS = `{"models":[{"name":"vllm/qwen3.8:27b","model":"vllm/qwen3.8:27b","context_length":100000}]}`

func TestDiscoverModelWindow_OllamaReportsTheServedWindow(t *testing.T) {
	var completions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(ollamaPS))
		default:
			completions.Add(1)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "vllm/qwen3.8:27b")
	if err != nil || n != 100000 {
		t.Fatalf("want the served 100000, got %d (%v)", n, err)
	}
	if completions.Load() != 0 {
		t.Fatal("an answered /api/ps must not run the refusal probe")
	}
}

func TestDiscoverModelWindow_OllamaModelNotLoadedIsUndiscoveredWithoutGenerating(t *testing.T) {
	var completions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.34.4"}`))
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			completions.Add(1)
		}
	}))
	defer srv.Close()
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "vllm/qwen3.8:27b")
	if n != 0 || !errors.Is(err, ErrOllamaModelNotLoaded) {
		t.Fatalf("want undiscovered with the reason, got %d (%v)", n, err)
	}
	if !strings.Contains(err.Error(), "vllm/qwen3.8:27b") {
		t.Fatalf("the reason must name the model: %v", err)
	}
	if completions.Load() != 0 {
		t.Fatal("on Ollama the refusal probe would load the model and generate; it must not run")
	}
}

func TestDiscoverModelWindow_NotOllamaFallsBackToTheRefusalProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"max_tokens=99000000 cannot be greater than max_model_len=32768"}`))
	}))
	defer srv.Close()
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err != nil || n != 32768 {
		t.Fatalf("want the refusal probe's 32768, got %d (%v)", n, err)
	}
}

// A server with a "models" list that is not Ollama-shaped is not Ollama.
func TestDiscoverModelWindow_NonOllamaModelsListFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			_, _ = w.Write([]byte(`{"models":[{"id":"m"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`max_model_len=65536`))
	}))
	defer srv.Close()
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err != nil || n != 65536 {
		t.Fatalf("a non-Ollama models list must fall back to the probe: %d (%v)", n, err)
	}
}

// An alias whose ORIGINAL is loaded appears in /api/ps under the original's
// name; they share a digest (benchmark LLD §12.11.3, aliases, 2026-09-26).
// Found on the qwen3.6:35b arm: aliased vllm/qwen3.6:35b, listed as qwen3.6:35b.
func TestDiscoverModelWindow_AliasMatchesByDigest(t *testing.T) {
	var completions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3.6:35b","model":"qwen3.6:35b","digest":"07d35212591f","context_length":100000}]}`))
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"vllm/qwen3.6:35b","digest":"07d35212591f"},{"name":"qwen3.6:35b","digest":"07d35212591f"}]}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.34.4"}`))
		default:
			completions.Add(1)
		}
	}))
	defer srv.Close()
	n, err := DiscoverModelWindow(context.Background(), srv.URL+"/v1", "", "vllm/qwen3.6:35b")
	if err != nil || n != 100000 {
		t.Fatalf("an alias of a loaded model must resolve by digest: %d (%v)", n, err)
	}
	if completions.Load() != 0 {
		t.Fatal("no refusal probe")
	}
}

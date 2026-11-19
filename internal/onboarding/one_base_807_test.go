package onboarding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// GitHub #807 (security design 18, amendment 2026-11-18 item 3): one base,
// ending in /v1, serves both clients. The chat probe lists models and pings a
// completion under it; the embeddings probe posts under the same base.
// Before, the embedder posted to <base>/v1/embeddings, so a /v1 base failed.
func TestOneBaseServesChatAndEmbeddings_807(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-m"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base := srv.URL + "/v1"
	chat := NewChatValidator().Validate(context.Background(), ChatConfigProposal{Endpoint: base, APIKey: "k", Model: "chat-m"})
	if !chat.PingOK || !chat.ModelKnown {
		t.Fatalf("chat against %s: %+v", base, chat)
	}
	mem := NewMemoryValidator().Validate(context.Background(), MemoryConfigProposal{Enabled: true, EmbeddingEndpoint: base, EmbeddingAPIKey: "k", EmbeddingModel: "e"})
	if !mem.EmbeddingOK || mem.ReturnedDimension != 2 {
		t.Fatalf("embeddings against %s: %+v", base, mem)
	}
	mu.Lock()
	defer mu.Unlock()
	if paths["/v1/v1/embeddings"] || !paths["/v1/embeddings"] || !paths["/v1/chat/completions"] {
		t.Fatalf("paths requested: %v", paths)
	}
}

package featuredoctor

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// stubEmbeddingProber stands in for a live probe against the dedicated
// memory.embedding_endpoint. reachable is what ProbeEmbedding returns.
type stubEmbeddingProber struct{ reachable bool }

func (s stubEmbeddingProber) ProbeEmbedding(context.Context, memory.Config) bool {
	return s.reachable
}

// findMemoryRAGPrereq returns the named prereq from the memory-rag feature
// or fails the test if it's absent.
func findMemoryRAGPrereq(t *testing.T, name string) Prereq {
	t.Helper()
	f := memoryRAGFeature()
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == name {
			return f.Prereqs[i]
		}
	}
	t.Fatalf("missing %q prereq", name)
	return Prereq{}
}

func TestMemoryRAGPrereq_EmbeddingModel(t *testing.T) {
	// No dedicated endpoint configured → embeddings fall back to the
	// agent/chat endpoint, so the chat-provider catalog (ModelPinger) is
	// the correct surface. Unreachable there must be unmet+unfixable.
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"memory.embedding_model": "nomic-embed"}},
		Models: stubModels{reachable: false},
	}
	res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
	if res.OK || res.Fixable {
		t.Fatalf("unreachable embedding model must be unmet+unfixable, got %+v", res)
	}
}

// TestMemoryRAGPrereq_EmbeddingModelProbesDedicatedEndpoint is the
// regression guard for the 2026-06-12 incident: a locally-served
// bge-m3:latest on memory.embedding_endpoint (127.0.0.1:11434) was
// reported "not reachable" because the doctor only consulted the
// chat-provider catalog (ModelPinger), which never lists the dedicated
// embedding endpoint's models. When an endpoint IS configured the check
// must probe THAT endpoint, not the chat catalog.
func TestMemoryRAGPrereq_EmbeddingModelProbesDedicatedEndpoint(t *testing.T) {
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"memory.embedding_model":    "bge-m3:latest",
			"memory.embedding_endpoint": "http://127.0.0.1:11434",
		}},
		Models:     stubModels{reachable: false}, // chat catalog does NOT list it
		Embeddings: stubEmbeddingProber{reachable: true},
	}
	res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("embedding model served at the dedicated endpoint must be reachable, got %+v", res)
	}
}

func TestMemoryRAGPrereq_BedrockModelReachable(t *testing.T) {
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"memory.embedding_provider": "bedrock",
			"memory.embedding_model":    "amazon.titan-embed-text-v2:0",
			"memory.bedrock.region":     "eu-central-1",
		}},
		Embeddings: stubEmbeddingProber{reachable: true},
	}
	res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("bedrock embedding model must probe reachable, got %+v", res)
	}
}

func TestMemoryRAGPrereq_EmbeddingModelUnreachableAtEndpoint(t *testing.T) {
	// Endpoint configured but the model is not served there → unmet,
	// unfixable, and the remediation must name the endpoint.
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"memory.embedding_model":    "bge-m3:latest",
			"memory.embedding_endpoint": "http://127.0.0.1:11434",
		}},
		Models:     stubModels{reachable: true}, // chat catalog membership must be ignored
		Embeddings: stubEmbeddingProber{reachable: false},
	}
	res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
	if res.OK || res.Fixable {
		t.Fatalf("model absent at endpoint must be unmet+unfixable, got %+v", res)
	}
}

func TestMemoryRAGPrereq_EmbeddingEndpointSetButProberMissing(t *testing.T) {
	// Endpoint configured but no prober wired → cannot claim reachable.
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"memory.embedding_model":    "bge-m3:latest",
			"memory.embedding_endpoint": "http://127.0.0.1:11434",
		}},
		Models: stubModels{reachable: true},
	}
	res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
	if res.OK {
		t.Fatalf("missing embedding prober must not report reachable, got %+v", res)
	}
}

// GitHub #807: an endpoint in the old form (no /v1) still works, and the
// doctor names the one form both clients now take.
func TestMemoryRAGPrereq_LegacyEmbeddingEndpointNamesTheNewForm_807(t *testing.T) {
	for endpoint, wantNote := range map[string]bool{"https://openrouter.ai/api": true, "https://openrouter.ai/api/v1": false} {
		deps := Deps{
			Config: stubConfig{vals: map[string]any{
				"memory.embedding_model":    "text-embedding-3-small",
				"memory.embedding_endpoint": endpoint,
			}},
			Embeddings: stubEmbeddingProber{reachable: true},
		}
		res := findMemoryRAGPrereq(t, "embedding model reachable").Check(context.Background(), deps)
		gotNote := strings.Contains(res.Detail, "old form") && strings.Contains(res.Detail, "https://openrouter.ai/api/v1")
		if !res.OK || gotNote != wantNote {
			t.Errorf("%s: %+v (note of the /v1 form: %v, want %v)", endpoint, res, gotNote, wantNote)
		}
	}
}

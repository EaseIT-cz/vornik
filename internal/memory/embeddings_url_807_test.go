package memory

import "testing"

// GitHub #807 (security design 18, amendment 2026-11-18 item 3): the chat
// client takes the OpenAI-compatible base ending in /v1, while the embedder
// appended /v1/embeddings to any base, so one provider needed two strings
// (OpenRouter: .../api/v1 for chat, .../api for embeddings) and a /v1 base
// failed as .../v1/v1/embeddings. Found at: production (vornik-cloud
// staging). Lower because: the URL is built in one function; the page test
// covers what the owner types.
func TestEmbeddingsURL_OneBaseForBothClients_807(t *testing.T) {
	for base, want := range map[string]string{
		"https://openrouter.ai/api/v1":  "https://openrouter.ai/api/v1/embeddings",
		"https://openrouter.ai/api/v1/": "https://openrouter.ai/api/v1/embeddings",
		"https://api.example.com/V1":    "https://api.example.com/V1/embeddings",
		// The old form still works: existing configs keep their URL.
		"https://openrouter.ai/api": "https://openrouter.ai/api/v1/embeddings",
		"http://127.0.0.1:11434":    "http://127.0.0.1:11434/v1/embeddings",
	} {
		if got := EmbeddingsURL(base); got != want {
			t.Errorf("EmbeddingsURL(%q) = %q, want %q", base, got, want)
		}
	}
	if !EmbeddingBaseIsLegacy("https://openrouter.ai/api") || EmbeddingBaseIsLegacy("https://openrouter.ai/api/v1") {
		t.Error("the legacy form is a base that does not end in /v1")
	}
}

package onboarding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub #807 (security design 18, amendment 2026-11-18 item 1): the owner's
// embeddings probe dials through the managed guard, as the chat probe does,
// so a loopback or private endpoint is refused before any request is made.
func TestManagedMemoryValidator_RefusesAPrivateEndpoint_807(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()
	p := MemoryConfigProposal{Enabled: true, EmbeddingEndpoint: srv.URL + "/v1", EmbeddingModel: "m", EmbeddingAPIKey: "k"}
	if res := NewManagedMemoryValidator(nil).Validate(context.Background(), p); res.EmbeddingOK || hits != 0 {
		t.Fatalf("the managed probe reached a loopback endpoint: %+v (requests %d)", res, hits)
	}
	// The setup wizard's validator, for an operator, may reach it.
	if res := NewMemoryValidator().Validate(context.Background(), p); !res.EmbeddingOK || res.ReturnedDimension != 3 {
		t.Fatalf("the operator's probe could not reach a loopback endpoint: %+v", res)
	}
}

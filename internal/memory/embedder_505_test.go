package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #505 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#505)",
// C4): the embedder's default client was a bare &http.Client{Timeout: 60s}, so
// a configured embedding endpoint naming, or redirecting to, the cloud
// metadata service reached it. Found at: review (394c F1). Integration level:
// a real HTTP server and the real embedder.

func embedProbe(t *testing.T, endpoint string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := NewEmbedder(Config{EmbeddingEndpoint: endpoint, EmbeddingModel: "m"})
	_, err := e.Embed(ctx, EmbedScope{CallSite: EmbedCallSiteInfraProbe}, []string{"x"})
	return err
}

func TestEmbedder505_MetadataEndpointRefused(t *testing.T) {
	if err := embedProbe(t, "http://169.254.169.254:80/"); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("embedding endpoint on the metadata address: err = %v, want dialguard.ErrDialRefused", err)
	}
}

func TestEmbedder505_RedirectToMetadataRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254:80/latest/meta-data/", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if err := embedProbe(t, srv.URL+"/"); !errors.Is(err, dialguard.ErrCrossHostRedirect) {
		t.Fatalf("embedder followed a redirect to the metadata service: err = %v, want dialguard.ErrCrossHostRedirect", err)
	}
}

// GitHub #560 (M1c D0): an embedder built before managed hosting's policy was
// set dials under it.
func TestEmbedder560_BuiltBeforePolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL + "/", EmbeddingModel: "m"})
	dialguard.SetManagedPolicy(&dialguard.ManagedPolicy{})
	t.Cleanup(func() { dialguard.SetManagedPolicy(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.Embed(ctx, EmbedScope{CallSite: EmbedCallSiteInfraProbe}, []string{"x"}); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("managed: an embedder built before the policy dialled loopback: %v", err)
	}
}

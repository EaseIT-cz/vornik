package onboarding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #502 (18-security-and-tenancy.md, "Amendment 2026-10-22 (#502)",
// S3): the setup probes dialled the proposed endpoint with an unguarded
// client that consulted HTTP(S)_PROXY and followed redirects, so a probe
// could be bounced to the cloud metadata service. Both probes (the chat
// list and ping, the embedding probe) now dial through setupProbeHTTPClient.
// Found at: review. A user meets it on POST /api/v1/setup/models; the gate
// half is tested there (api, TestSetupSurface502_*). The dial half is tested
// here because the address a caller supplies cannot be steered to
// 169.254.169.254 from a test.

func TestSetupProbeHTTPClient_IsGuarded_Issue502(t *testing.T) {
	c := setupProbeHTTPClient(setupProbeTimeout)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("probe transport is %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("probe transport consults a proxy: the guarded dial would see the proxy, not the target")
	}
	if c.CheckRedirect == nil {
		t.Error("probe client follows redirects")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:80"); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Errorf("probe dial to the metadata address = %v, want ErrDialRefused", err)
	}
}

// redirectPair502 is an upstream that answers every request with a redirect
// to a second server, and a count of the requests that second server got.
func redirectPair502(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(up.Close)
	return up.URL, &reached
}

func TestChatValidator_ListModelsDoesNotFollowRedirects_Issue502(t *testing.T) {
	up, reached := redirectPair502(t)
	if _, err := NewChatValidator().ListModels(context.Background(), up+"/v1", "k"); err == nil {
		t.Error("ListModels succeeded through a redirect")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect target was reached %d times: the chat probe follows redirects", n)
	}
}

func TestMemoryValidator_ProbeDoesNotFollowRedirects_Issue502(t *testing.T) {
	up, reached := redirectPair502(t)
	res := NewMemoryValidator().Validate(context.Background(), MemoryConfigProposal{
		Enabled: true, EmbeddingEndpoint: up + "/v1", EmbeddingModel: "m", EmbeddingDimension: 8,
	})
	_ = res
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect target was reached %d times: the embedding probe follows redirects", n)
	}
}

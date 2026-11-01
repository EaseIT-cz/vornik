package chat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #505 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#505)"):
// the clients the daemon builds from its configured chat endpoint dialled
// through a plain clone of http.DefaultTransport (proxy from the environment,
// no address check, redirects to any host), so an endpoint naming, or
// redirecting to, the cloud metadata service reached it. Found at: review
// (394c F1). These tests sit at integration level: a real HTTP server and the
// real client.

const metadataURL = "http://169.254.169.254:80/latest/meta-data/"

var hi = []Message{{Role: "user", Content: "hi"}}

func okCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
}

func okStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// An endpoint on the metadata address is refused at connect time, on the
// buffered and the streaming path.
func TestModelClient505_MetadataEndpointRefused(t *testing.T) {
	c := NewClient("http://169.254.169.254:80/v1", "", "m", WithTimeout(5*time.Second))
	if _, err := c.Complete(shortCtx(t), hi); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("Complete to the metadata address: err = %v, want dialguard.ErrDialRefused", err)
	}
	if _, err := c.CompleteWithToolsStream(shortCtx(t), hi, nil, nil); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("stream to the metadata address: err = %v, want dialguard.ErrDialRefused", err)
	}
}

// A guard refusal is permanent, so the client does not retry it: one request,
// not the retry loop's three with backoff.
func TestModelClient505_RefusalNotRetried(t *testing.T) {
	var calls atomic.Int32
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, dialguard.ErrDialRefused
	})
	c := NewClient("https://models.example.invalid/v1", "", "m", WithHTTPClient(&http.Client{Transport: rt}))
	if _, err := c.Complete(shortCtx(t), hi); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("err = %v, want dialguard.ErrDialRefused", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a refused dial was attempted %d times, want 1", n)
	}
}

// A loopback server answering with a redirect to the metadata service: the
// hop is refused and the metadata address is never dialled.
func TestModelClient505_RedirectToMetadataRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, metadataURL, http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v1", "", "m", WithTimeout(5*time.Second))
	if _, err := c.Complete(shortCtx(t), hi); !errors.Is(err, dialguard.ErrCrossHostRedirect) {
		t.Fatalf("Complete followed a redirect to the metadata service: err = %v, want dialguard.ErrCrossHostRedirect", err)
	}
	if _, err := c.CompleteWithToolsStream(shortCtx(t), hi, nil, nil); !errors.Is(err, dialguard.ErrCrossHostRedirect) {
		t.Fatalf("stream followed a redirect to the metadata service: err = %v, want dialguard.ErrCrossHostRedirect", err)
	}
}

// A same-host redirect (a gateway moving a path) is still followed; a redirect
// to another host is refused before that host is dialled.
func TestModelClient505_SameHostRedirectFollowed(t *testing.T) {
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old/") {
			http.Redirect(w, r, "/v1/chat/completions", http.StatusTemporaryRedirect)
			return
		}
		okCompletion(w)
	}))
	defer same.Close()
	if _, err := NewClient(same.URL+"/old", "", "m").Complete(shortCtx(t), hi); err != nil {
		t.Fatalf("a same-host redirect was not followed: %v", err)
	}

	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		okCompletion(w)
	}))
	defer other.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer hop.Close()
	if _, err := NewClient(hop.URL+"/v1", "", "m").Complete(shortCtx(t), hi); !errors.Is(err, dialguard.ErrCrossHostRedirect) {
		t.Fatalf("cross-host redirect: err = %v, want dialguard.ErrCrossHostRedirect", err)
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the redirect's second host received %d request(s), want 0", n)
	}
}

// Regression guard: a loopback model server (Ollama on the host, a LAN
// gateway) stays reachable on both paths.
func TestModelClient505_LoopbackServerStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = io.Copy(&body, r.Body)
		if strings.Contains(body.String(), `"stream":true`) {
			okStream(w)
			return
		}
		okCompletion(w)
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "", "m")
	if _, err := c.Complete(shortCtx(t), hi); err != nil {
		t.Fatalf("loopback Complete: %v", err)
	}
	if _, err := c.CompleteWithToolsStream(shortCtx(t), hi, nil, nil); err != nil {
		t.Fatalf("loopback stream: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The shared transport ignores proxy variables and dials through the guard;
// every client built on it refuses cross-host redirects; and the streaming
// path uses the client's own transport, so a client set with WithHTTPClient
// is the one that streams (C3).
func TestModelClient505_ProxyIgnored(t *testing.T) {
	tr := newChatTransport()
	if tr.Proxy != nil {
		t.Error("the shared chat transport consults a proxy: the guard would check the proxy's address, not the target's")
	}
	if tr.DialContext == nil {
		t.Error("the shared chat transport has no guarded dial")
	}

	clients := map[string]*http.Client{
		"NewClient default":         NewClient("https://example.invalid", "", "m").httpClient,
		"Claude subscription":       NewClaudeSubscriptionClient("m").http,
		"Codex subscription":        NewCodexSubscriptionClient("m").http,
		"Claude OAuth token client": newClaudeAuthManager(t.TempDir() + "/c.json").http,
		"Codex OAuth token client":  newCodexAuthManager(t.TempDir() + "/c.json").httpClient,
	}
	for name, hc := range clients {
		if hc.CheckRedirect == nil {
			t.Errorf("%s follows redirects to any host (no CheckRedirect)", name)
		}
	}

	var used atomic.Int32
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		used.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	c := NewClient("https://models.example.invalid/v1", "", "m", WithHTTPClient(&http.Client{Transport: rt}))
	if _, err := c.CompleteWithToolsStream(shortCtx(t), hi, nil, nil); err != nil {
		t.Fatalf("stream through the client's own transport: %v", err)
	}
	if used.Load() != 1 {
		t.Fatalf("the stream did not use the client set with WithHTTPClient (its transport ran %d times)", used.Load())
	}
}

// At start, set proxy variables are named in one warning, never their values.
func TestModelClient505_ProxyEnvWarning(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	var buf bytes.Buffer
	LogIgnoredProxyEnv(zerolog.New(&buf))
	if buf.Len() != 0 {
		t.Fatalf("no proxy variable set, but a warning was logged: %s", buf.String())
	}

	t.Setenv("HTTPS_PROXY", "http://user:s3cr3t-value@proxy.internal:3128")
	LogIgnoredProxyEnv(zerolog.New(&buf))
	out := buf.String()
	if !strings.Contains(out, "HTTPS_PROXY") {
		t.Fatalf("the warning does not name the variable: %s", out)
	}
	if strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "proxy.internal") {
		t.Fatalf("the warning prints the variable's value: %s", out)
	}
}

// GitHub #560 (M1c D0): the shared transport resolves the guard on every dial,
// so a transport built before managed hosting's policy was set dials under it.
func TestModelClient560_TransportReadsPolicyLive(t *testing.T) {
	tr := newChatTransport() // built while self-hosted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { okCompletion(w) }))
	defer srv.Close()
	dialguard.SetManagedPolicy(&dialguard.ManagedPolicy{})
	t.Cleanup(func() { dialguard.SetManagedPolicy(nil) })
	c := NewClient(srv.URL+"/v1", "", "m", WithHTTPClient(&http.Client{Transport: tr, CheckRedirect: dialguard.SameHostRedirects}))
	if _, err := c.Complete(shortCtx(t), hi); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("managed: a loopback model endpoint was dialled through a transport built before the policy: %v", err)
	}
}

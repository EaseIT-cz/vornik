package memory

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// GitHub #369 item 5 (outcome-quality §S8.3 #5, §S8.10): the embedder answered
// "no vector" with NO error for ~16 causes, so callers could not tell a rejected
// key from a dead host from a dimension mismatch, and the embed DLQ stored an
// empty last_error. Every cause now returns a typed *EmbedError whose Reason
// names the remedy. Only zero inputs keep (nil, nil): nothing failed.

func openAICompatServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func assertEmbedReason(t *testing.T, vecs [][]float32, err error, want string) *EmbedError {
	t.Helper()
	if vecs != nil {
		t.Fatalf("vectors returned on a failure: %v", vecs)
	}
	if err == nil {
		t.Fatalf("no error: the (nil, nil) degrade of #369 item 5; want reason %q", want)
	}
	got, ok := EmbedFailureReason(err)
	if !ok {
		t.Fatalf("error %q is not a typed EmbedError; want reason %q", err, want)
	}
	if got != want {
		t.Fatalf("reason = %q, want %q (err %q)", got, want, err)
	}
	var ee *EmbedError
	_ = errors.As(err, &ee)
	return ee
}

func TestEmbed369_EveryOpenAICompatCauseIsTyped(t *testing.T) {
	vec := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		dim     int
		texts   []string
		want    string
	}{
		{"http 401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, 0, []string{"x"}, EmbedReasonHTTPStatus},
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, 0, []string{"x"}, EmbedReasonHTTPStatus},
		{"not json", vec("not json"), 0, []string{"x"}, EmbedReasonDecode},
		{"provider error object", vec(`{"error":{"message":"rate limited"}}`), 0, []string{"x"}, EmbedReasonProviderError},
		{"duplicate index", vec(`{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`), 0, []string{"a", "b"}, EmbedReasonMalformedResponse},
		{"empty vector", vec(`{"data":[{"index":0,"embedding":[]}]}`), 0, []string{"a"}, EmbedReasonEmptyVector},
		{"dimension mismatch", vec(`{"data":[{"index":0,"embedding":[1,2]}]}`), 3, []string{"a"}, EmbedReasonDimensionMismatch},
		{"missing index", vec(`{"data":[{"index":0,"embedding":[1]}]}`), 0, []string{"a", "b"}, EmbedReasonPartialResponse},
		{"no data at all", vec(`{}`), 0, []string{"a"}, EmbedReasonPartialResponse},
		{"body cut short", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":`))
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
		}, 0, []string{"a"}, EmbedReasonReadBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := openAICompatServer(t, tc.handler)
			e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "m", EmbeddingDimension: tc.dim})
			vecs, err := e.Embed(context.Background(), ingestScope(), tc.texts)
			_ = assertEmbedReason(t, vecs, err, tc.want)
		})
	}
}

func TestEmbed369_HTTPStatusCarriesTheCode(t *testing.T) {
	url := openAICompatServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "m"})
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"x"})
	ee := assertEmbedReason(t, vecs, err, EmbedReasonHTTPStatus)
	if ee.Status != http.StatusForbidden || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("status = %d, message %q: want 403 named in both", ee.Status, err)
	}
}

func TestEmbed369_DimensionMismatchCarriesBothWidths(t *testing.T) {
	url := openAICompatServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
	})
	e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "m", EmbeddingDimension: 3})
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"x"})
	ee := assertEmbedReason(t, vecs, err, EmbedReasonDimensionMismatch)
	if ee.Got != 2 || ee.Want != 3 {
		t.Fatalf("got/want = %d/%d, want 2/3", ee.Got, ee.Want)
	}
}

func TestEmbed369_TransportTimeoutAndCancel(t *testing.T) {
	// Transport: nothing listens.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: dead, EmbeddingModel: "m"})
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"x"})
	_ = assertEmbedReason(t, vecs, err, EmbedReasonTransport)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hang := openAICompatServer(t, func(http.ResponseWriter, *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	e = NewEmbedder(Config{EmbeddingEndpoint: hang, EmbeddingModel: "m"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	vecs, err = e.Embed(ctx, ingestScope(), []string{"x"})
	_ = assertEmbedReason(t, vecs, err, EmbedReasonTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout does not wrap context.DeadlineExceeded: %v", err)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); ccancel() }()
	vecs, err = e.Embed(cctx, ingestScope(), []string{"x"})
	_ = assertEmbedReason(t, vecs, err, EmbedReasonCancelled)
}

func TestEmbed369_NotConfiguredIsTypedAndZeroInputsAreNot(t *testing.T) {
	e := NewEmbedder(Config{})
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"x"})
	_ = assertEmbedReason(t, vecs, err, EmbedReasonNotConfigured)

	v, err := e.EmbedQuery(context.Background(), ingestScope(), "q")
	if v != nil || err == nil {
		t.Fatalf("EmbedQuery unconfigured: vec=%v err=%v, want a typed error", v, err)
	}
	if r, _ := EmbedFailureReason(err); r != EmbedReasonNotConfigured {
		t.Fatalf("EmbedQuery reason = %q", r)
	}

	// Zero inputs: zero vectors IS the full answer. Not a failure, not counted.
	ok := NewEmbedder(Config{EmbeddingEndpoint: "http://unused.invalid", EmbeddingModel: "m"})
	vecs, err = ok.Embed(context.Background(), ingestScope(), nil)
	if vecs != nil || err != nil {
		t.Fatalf("zero inputs: %v %v, want (nil, nil)", vecs, err)
	}
	v, err = ok.EmbedQuery(context.Background(), ingestScope(), "  ")
	if v != nil || err != nil {
		t.Fatalf("empty query: %v %v, want (nil, nil)", v, err)
	}
}

func TestEmbed369_BedrockCausesAreTyped(t *testing.T) {
	titan := func(c bedrockRuntimeEmbedClient) *Embedder {
		e := NewEmbedder(Config{EmbeddingProvider: "bedrock", EmbeddingModel: "amazon.titan-embed-text-v2:0", BedrockRegion: "eu-central-1"})
		e.bedrockClient = c
		e.bedrockInitErr = nil
		return e
	}
	for _, tc := range []struct {
		name string
		e    *Embedder
		want string
	}{
		{"no region", NewEmbedder(Config{EmbeddingProvider: "bedrock", EmbeddingModel: "amazon.titan-embed-text-v2:0", BedrockRegion: "eu-central-1", EmbeddingAPIKey: ""}).withInitErr(errors.New("load aws config")), EmbedReasonBackendInit},
		{"titan invoke error", titan(fakeBedrockEmbedClient{err: errors.New("AccessDenied")}), EmbedReasonTransport},
		{"titan empty vector", titan(fakeBedrockEmbedClient{body: []byte(`{"embedding":[]}`)}), EmbedReasonEmptyVector},
		{"titan bad body", titan(fakeBedrockEmbedClient{body: []byte(`nope`)}), EmbedReasonDecode},
		{"unsupported model", func() *Embedder {
			e := titan(fakeBedrockEmbedClient{body: []byte(`{"embedding":[1]}`)})
			e.cfg.EmbeddingModel = "meta.llama-not-an-embedder"
			return e
		}(), EmbedReasonUnsupportedModel},
		{"cohere invoke error", newCohereEmbedder(t, fakeBedrockEmbedClient{err: errors.New("ThrottlingException")}), EmbedReasonTransport},
		{"cohere count mismatch", newCohereEmbedder(t, fakeBedrockEmbedClient{body: cohereReply()}), EmbedReasonPartialResponse},
		{"cohere bad body", newCohereEmbedder(t, fakeBedrockEmbedClient{body: []byte(`nope`)}), EmbedReasonDecode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vecs, err := tc.e.Embed(context.Background(), ingestScope(), []string{"x"})
			_ = assertEmbedReason(t, vecs, err, tc.want)
		})
	}

	// The Cohere query path reaches the provider without Embed, and as built
	// skipped embedBatchBedrock's client check: with no client it called a nil
	// interface (a panic) instead of reporting the backend.
	broken := newCohereEmbedder(t, nil).withInitErr(errors.New("load aws config"))
	if v, err := broken.EmbedQuery(context.Background(), ingestScope(), "q"); v != nil {
		t.Fatalf("vector with no client: %v", v)
	} else if r, _ := EmbedFailureReason(err); r != EmbedReasonBackendInit {
		t.Fatalf("cohere query with no client: reason %q (err %v), want backend_init", r, err)
	}

	e := newCohereEmbedder(t, fakeBedrockEmbedClient{err: errors.New("ThrottlingException")})
	v, err := e.EmbedQuery(context.Background(), ingestScope(), "q")
	if v != nil {
		t.Fatalf("vector on failure: %v", v)
	}
	if r, ok := EmbedFailureReason(err); !ok || r != EmbedReasonTransport {
		t.Fatalf("cohere query reason = %q ok=%v err=%v", r, ok, err)
	}
}

// withInitErr is a test helper: a Bedrock embedder whose client could not be
// built (the region or the AWS config), without depending on the host's AWS env.
func (e *Embedder) withInitErr(err error) *Embedder {
	e.bedrockClient = nil
	e.bedrockInitErr = err
	return e
}

func TestEmbed369_FailuresAreCountedOncePerCallBySiteAndReason(t *testing.T) {
	url := openAICompatServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "m"})
	m := freshMetrics()
	e.setMetrics(m)

	_, _ = e.Embed(context.Background(), ingestScope(), []string{"a", "b"})
	_, _ = e.EmbedQuery(context.Background(), EmbedScope{ProjectID: "p", CallSite: EmbedCallSiteSearchQuery}, "q")
	_, _ = e.Embed(context.Background(), ingestScope(), nil) // zero inputs: not a failure

	if got := testutil.ToFloat64(m.EmbedFailuresTotal.WithLabelValues(EmbedCallSiteIngest, EmbedReasonHTTPStatus)); got != 1 {
		t.Fatalf("ingest http_status = %v, want 1", got)
	}
	// EmbedQuery goes through Embed on this provider: one call, one count.
	if got := testutil.ToFloat64(m.EmbedFailuresTotal.WithLabelValues(EmbedCallSiteSearchQuery, EmbedReasonHTTPStatus)); got != 1 {
		t.Fatalf("search_query http_status = %v, want 1 (counted twice?)", got)
	}

	c := newCohereEmbedder(t, fakeBedrockEmbedClient{err: errors.New("ThrottlingException")})
	c.setMetrics(m)
	_, _ = c.EmbedQuery(context.Background(), EmbedScope{ProjectID: "p", CallSite: EmbedCallSiteSearchQuery}, "q")
	if got := testutil.ToFloat64(m.EmbedFailuresTotal.WithLabelValues(EmbedCallSiteSearchQuery, EmbedReasonTransport)); got != 1 {
		t.Fatalf("cohere query transport = %v, want 1", got)
	}
}

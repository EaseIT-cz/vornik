package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #807 (security design 18, amendment 2026-11-18 item 2). Found at:
// production (vornik-cloud staging): after vornik-cloud's setup commit the
// instance is no longer fresh, so an own-key owner landed on a dashboard and
// a chat page that said "no chat dispatcher wired", with no way to the
// model-provider page but Manage. Regression test at: integration (the real
// console handler and session middleware).

func server807(managed bool, opts ...ServerOption) *Server {
	return NewServer(append([]ServerOption{WithLogger(zerolog.New(&bytes.Buffer{})), WithManagedHosting(managed)}, opts...)...)
}

func TestModelProvider807_OwnerWithoutChatIsSentToThePage(t *testing.T) {
	// No onboarding detector: the instance is past setup, never "fresh".
	rec := doConsole794(t, server807(true), http.MethodGet, "/", owner754, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/model-provider" {
		t.Fatalf("managed owner without chat: %d -> %q, want 302 /ui/model-provider", rec.Code, rec.Header().Get("Location"))
	}
	// Once a restart has wired chat, the dashboard is the dashboard.
	rec = doConsole794(t, server807(true, WithChatDispatcher(&stubChatDispatcher{})), http.MethodGet, "/", owner754, nil)
	if rec.Header().Get("Location") == "/ui/model-provider" {
		t.Fatal("an owner whose chat is wired is still redirected")
	}
	// A self-hosted instance is untouched.
	rec = doConsole794(t, server807(false), http.MethodGet, "/", admin794, nil)
	if rec.Header().Get("Location") == "/ui/model-provider" {
		t.Fatal("a self-hosted operator is sent to the managed owner's page")
	}
}

func TestModelProvider807_ChatNoticeLinksThePage(t *testing.T) {
	managed := server807(true).chatPageData(httptestRequest807(), "assistant")
	if !managed.Disabled || !managed.ConnectProvider || !strings.Contains(managed.Error, "No model is connected yet") {
		t.Fatalf("managed chat without a dispatcher: %+v", managed)
	}
	self := server807(false).chatPageData(httptestRequest807(), "assistant")
	if self.ConnectProvider || !strings.Contains(self.Error, "no chat dispatcher wired") {
		t.Fatalf("self-hosted chat without a dispatcher: %+v", self)
	}
}

func httptestRequest807() *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/ui/projects/assistant/chat", nil)
	return r
}

// memProbe807 fakes the managed embeddings probe.
type memProbe807 struct {
	res   onboarding.MemoryValidationResult
	calls []onboarding.MemoryConfigProposal
}

func (p *memProbe807) Validate(_ context.Context, pr onboarding.MemoryConfigProposal) onboarding.MemoryValidationResult {
	p.calls = append(p.calls, pr)
	return p.res
}

func rig807(t *testing.T, res onboarding.MemoryValidationResult) (*rig794, *memProbe807) {
	t.Helper()
	r := newRig794(t, true)
	mp := &memProbe807{res: res}
	WithOwnerMemoryProbe(mp)(r.srv)
	return r, mp
}

func memForm807(extra map[string]string) url.Values {
	f := goodForm794()
	f.Set("embedding_model", "text-embedding-3-small")
	for k, v := range extra {
		f.Set(k, v)
	}
	return f
}

var memOK807 = onboarding.MemoryValidationResult{EmbeddingOK: true, ReturnedDimension: 1536}

// #807 item 1: memory on the same page, probed, the key sealed in the store,
// the memory block written, the audit row saying so.
func TestModelProvider807_SaveEnablesMemory(t *testing.T) {
	r, mp := rig807(t, memOK807)
	rec := r.do(t, http.MethodPost, owner754, memForm807(map[string]string{"embedding_api_key": "sk-embed-own"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(mp.calls) != 1 || mp.calls[0].EmbeddingEndpoint != goodEndpoint794 || mp.calls[0].EmbeddingAPIKey != "sk-embed-own" {
		t.Fatalf("embedding probe calls %+v (want the chat endpoint by default and the typed key)", mp.calls)
	}
	if !slices.Contains(r.creds.puts, api.OwnerEmbeddingAPIKeyName+"=sk-embed-own") {
		t.Fatalf("the embedding key was not sealed: %v", r.creds.puts)
	}
	file := r.file(t)
	for _, want := range []string{"enabled: true", "embedding_model: text-embedding-3-small", "embedding_dimension: 1536", "${" + api.OwnerEmbeddingAPIKeyName + "}"} {
		if !strings.Contains(file, want) {
			t.Errorf("config lacks %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "sk-embed-own") || strings.Contains(rec.Body.String(), "sk-embed-own") {
		t.Fatal("the embedding key reached the config or the page")
	}
	// No own key: memory refers to the chat key's name, nothing more sealed.
	r2, _ := rig807(t, memOK807)
	r2.do(t, http.MethodPost, owner754, memForm807(nil))
	if len(r2.creds.puts) != 1 || !strings.Contains(r2.file(t), "${"+api.OwnerModelAPIKeyName+"}") {
		t.Fatalf("without an embedding key: puts %v", r2.creds.puts)
	}
}

// #807 item 1: the save is one unit; a failed embeddings probe, a dimension
// that differs from the provider's, or a shadowing environment writes
// nothing at all.
func TestModelProvider807_RefusalsWriteNothing(t *testing.T) {
	cases := map[string]struct {
		res   onboarding.MemoryValidationResult
		extra map[string]string
		env   bool
	}{
		"probe failed":  {res: onboarding.MemoryValidationResult{EmbeddingOK: false}},
		"dimension":     {res: memOK807, extra: map[string]string{"embedding_dimension": "768"}},
		"env shadows":   {res: memOK807, extra: map[string]string{"embedding_api_key": "sk-x"}, env: true},
		"bad dimension": {res: memOK807, extra: map[string]string{"embedding_dimension": "abc"}},
	}
	for name, c := range cases {
		r, _ := rig807(t, c.res)
		if c.env {
			r.env[api.OwnerEmbeddingAPIKeyName] = "set"
		}
		before := r.file(t)
		rec := r.do(t, http.MethodPost, owner754, memForm807(c.extra))
		if rec.Code == http.StatusOK || len(r.creds.puts) != 0 || r.file(t) != before {
			t.Errorf("%s: status %d, puts %v, file changed %v", name, rec.Code, r.creds.puts, r.file(t) != before)
		}
	}
}

// #807 item 4: a base that cannot list models gets the nearby base that
// can, as a suggestion; nothing is rewritten.
func TestModelProvider807_DidYouMean(t *testing.T) {
	r := newRig794(t, true)
	r.probe.res = onboarding.ChatValidationResult{EndpointOK: false, ModelsListed: false}
	r.probe.listing = map[string]bool{"https://openrouter.example/api/v1": true}
	f := goodForm794()
	f.Set("endpoint", "https://openrouter.example/api")
	before := r.file(t)
	rec := r.do(t, http.MethodPost, owner754, f)
	if !strings.Contains(rec.Body.String(), "Did you mean https://openrouter.example/api/v1?") {
		t.Fatalf("no suggestion: %s", rec.Body.String())
	}
	if len(r.probe.listed) > 2 || r.file(t) != before || len(r.creds.puts) != 0 {
		t.Fatalf("asked %v; a suggestion must not write", r.probe.listed)
	}
}

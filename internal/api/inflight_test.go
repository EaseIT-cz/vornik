package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// GitHub #97 (2026-10-07 deploy cut an in-flight chat stream): the deploy
// idle gate waited only for RUNNING/LEASED tasks, so the 2026.10.5 restart
// force-closed an external POST /v1/chat/completions (client 502 after 91 s).
// The chat-proxy routes now count their in-flight requests for the gate to
// read, and refuse new ones with 503 once a drain has started. Design:
// https://docs.vornik.io §8.

// blockingProvider holds CompleteWithTools until release is closed.
type blockingProvider struct {
	stubProvider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingProvider() *blockingProvider {
	b := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	b.resp = okStub().resp
	return b
}

func (b *blockingProvider) CompleteWithTools(ctx context.Context, m []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.stubProvider.CompleteWithTools(ctx, m, tools)
}

func chatBody() *strings.Reader {
	return strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
}

func TestInflight_CountsOpenChatStream(t *testing.T) {
	prov := newBlockingProvider()
	s := &Server{logger: zerolog.Nop(), chatProvider: prov}
	h := s.trackLongRequest(s.ChatCompletions)

	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody()))
		done <- w.Code
	}()
	<-prov.entered
	if got := s.InflightChatProxy(); got != 1 {
		t.Fatalf("in-flight while the request is open = %d, want 1", got)
	}
	close(prov.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("request = %d, want 200", code)
	}
	if got := s.InflightChatProxy(); got != 0 {
		t.Fatalf("in-flight after the request ended = %d, want 0", got)
	}
}

func TestInflight_DrainRefusesNewChatRequestWith503(t *testing.T) {
	stub := okStub()
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}
	s.SetDraining(true)
	w := httptest.NewRecorder()
	s.trackLongRequest(s.ChatCompletions)(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", chatBody()))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("a drain refusal must carry Retry-After")
	}
	if !strings.Contains(w.Body.String(), "DRAINING") {
		t.Fatalf("body lacks the DRAINING code: %s", w.Body.String())
	}
	if stub.lastCtx != nil {
		t.Fatal("the provider was called for a refused request")
	}
	if got := s.InflightChatProxy(); got != 0 {
		t.Fatalf("a refused request was counted: %d", got)
	}
}

func TestInflight_DrainLetsOpenStreamFinish(t *testing.T) {
	prov := newBlockingProvider()
	s := &Server{logger: zerolog.Nop(), chatProvider: prov}
	h := s.trackLongRequest(s.ChatCompletions)
	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody()))
		done <- w.Code
	}()
	<-prov.entered
	s.SetDraining(true)
	close(prov.release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("the open request = %d after the drain began, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open request did not finish")
	}
	if got := s.InflightChatProxy(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
}

func TestInflight_PanicStillDecrements(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	h := s.trackLongRequest(func(http.ResponseWriter, *http.Request) { panic("boom") })
	func() {
		defer func() { _ = recover() }()
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	}()
	if got := s.InflightChatProxy(); got != 0 {
		t.Fatalf("in-flight after a panic = %d, want 0", got)
	}
}

func inflightRequest(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/daemon/inflight", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	w := httptest.NewRecorder()
	s.DaemonInflight(w, req.WithContext(ctx))
	return w
}

func TestInflight_EndpointRequiresAdminClass(t *testing.T) {
	s := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}))
	s.inflight.chatProxy.Add(2)

	if w := inflightRequest(t, s, "sk-tenant"); w.Code != http.StatusForbidden {
		t.Fatalf("a non-admin key = %d, want 403", w.Code)
	}
	if w := inflightRequest(t, s, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", w.Code)
	}
	w := inflightRequest(t, s, "sk-operator")
	if w.Code != http.StatusOK {
		t.Fatalf("the operator key = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		ChatProxy int64 `json:"chat_proxy"`
		Total     int64 `json:"total"`
		Draining  bool  `json:"draining"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ChatProxy != 2 || got.Total != 2 || got.Draining {
		t.Fatalf("body = %+v, want chat_proxy 2, total 2, draining false", got)
	}

	post := httptest.NewRequest(http.MethodPost, "/api/v1/daemon/inflight", nil)
	pw := httptest.NewRecorder()
	s.DaemonInflight(pw, post)
	if pw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", pw.Code)
	}
}

// The four long chat routes are wrapped at registration; a drained router
// refuses them, and a metadata route is untouched.
func TestInflight_RoutesWrapChatProxyAndOllamaChat(t *testing.T) {
	s := NewServer(WithChatProvider(okStub()))
	s.SetDraining(true)
	h := SetupRoutes(s, &config.Config{})
	for _, p := range []string{"/api/v1/chat/completions", "/v1/chat/completions", "/api/chat", "/api/generate"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, p, chatBody()))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "DRAINING") {
			t.Errorf("POST %s while draining = %d %s, want 503 DRAINING", p, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("GET /api/version is a metadata route and must not be refused")
	}
}

// Review 5860 #4: with auth off the endpoint answers (single-operator mode).
func TestInflight_EndpointAuthOffAnswers(t *testing.T) {
	s := NewServer()
	s.inflight.chatProxy.Add(1)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/daemon/inflight", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	w := httptest.NewRecorder()
	s.DaemonInflight(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("auth off = %d %s, want 200 with total 1", w.Code, w.Body.String())
	}
}

// Review 5860 #4: end to end through the real router and AuthMiddleware, an
// admin-class key reads the count and a non-admin key is refused.
func TestInflight_EndpointThroughRouterAdminAndNonAdminKey(t *testing.T) {
	s := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}))
	cfg := &config.Config{API: config.APIConfig{AuthEnabled: true, APIKeys: []string{"sk-operator", "sk-tenant"}}}
	h := SetupRoutes(s, cfg)
	get := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/daemon/inflight", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := get("sk-operator"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("admin key = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := get("sk-tenant"); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin key = %d %s, want 403", w.Code, w.Body.String())
	}
	if w := get(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", w.Code)
	}
}

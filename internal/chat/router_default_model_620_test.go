package chat

// GitHub #620 (nonswarm-llm-fallback design §11, 2026-11-05). Found at:
// production. During the Ollama quota outage the operator set
// chat.model: zai.glm-5 to move the dispatcher to Bedrock; the router pinned it
// onto router.default (ollama_cloud) instead of the bedrock prefix route, and
// every turn got 404 "model not found".

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinStub is a sub-provider that reports which sub-provider answered (ID) and
// which model it was pinned to (Model).
type pinStub struct {
	name  string
	model string
}

func (s *pinStub) resp() *ChatResponse { return &ChatResponse{ID: s.name, Model: s.model} }
func (s *pinStub) Complete(context.Context, []Message) (*ChatResponse, error) {
	return s.resp(), nil
}
func (s *pinStub) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return s.resp(), nil
}
func (s *pinStub) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return s.resp(), nil
}
func (s *pinStub) Model() string       { return s.model }
func (s *pinStub) SetMetrics(*Metrics) {}
func (s *pinStub) WithModel(m string) Provider {
	return &pinStub{name: s.name, model: m}
}

func newRouter620(t *testing.T, chatModel string) *Router {
	t.Helper()
	// The container pins router.default to chat.model before building the
	// router (initChatRouter); reproduce that.
	ollama := (&pinStub{name: "ollama_cloud", model: "glm-5.2"}).WithModel(chatModel)
	bedrock := &pinStub{name: "bedrock", model: "zai.glm-5"}
	r, err := NewRouter(ollama, []Route{
		{Prefix: "zai.", Provider: bedrock, Name: "bedrock"},
	}, WithRouterFallbackName("ollama_cloud"), WithRouterDefaultModel(chatModel))
	require.NoError(t, err)
	return r
}

func TestRouter_ChatModelRoutesByPrefix_620(t *testing.T) {
	r := newRouter620(t, "zai.glm-5")
	ctx := context.Background()

	for name, call := range map[string]func() (*ChatResponse, error){
		"Complete":                func() (*ChatResponse, error) { return r.Complete(ctx, nil) },
		"CompleteWithTools":       func() (*ChatResponse, error) { return r.CompleteWithTools(ctx, nil, nil) },
		"CompleteWithToolsStream": func() (*ChatResponse, error) { return r.CompleteWithToolsStream(ctx, nil, nil, nil) },
	} {
		resp, err := call()
		require.NoError(t, err, name)
		assert.Equal(t, "bedrock", resp.ID, "%s: chat.model must take its prefix route, not router.default", name)
		assert.Equal(t, "zai.glm-5", resp.Model, name)
	}
	assert.Equal(t, "zai.glm-5", r.Model())

	// An unrouted model that names itself still goes to router.default.
	assert.Equal(t, "ollama_cloud", dispatchModel(t, r, "deepseek-v4-pro:0813"))
}

func TestRouter_ChatModelOnDefaultIsUnchanged_620(t *testing.T) {
	r := newRouter620(t, "deepseek-v4-pro:0813")
	resp, err := r.Complete(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ollama_cloud", resp.ID)
	assert.Equal(t, "deepseek-v4-pro:0813", resp.Model)
}

func TestRouter_RouteOf_620(t *testing.T) {
	r := newRouter620(t, "deepseek-v4-pro:0813")
	assert.Equal(t, "bedrock", r.RouteOf("zai.glm-5"))
	assert.Equal(t, "ollama_cloud", r.RouteOf("deepseek-v4-pro:0813"), "unmatched resolves to router.default")
}

func TestModelFallbackFindings_620(t *testing.T) {
	routeOf := func(m string) string {
		if len(m) > 4 && m[:4] == "zai." {
			return "bedrock"
		}
		return "ollama_cloud"
	}
	cases := []struct {
		name      string
		chatModel string
		fallbacks map[string]string
		want      int
		contains  string
	}{
		{"empty map is not a finding", "deepseek-v4-pro:0813", nil, 0, ""},
		{"missing entry (the 2026-11-05 gap)", "deepseek-v4-pro:0813", map[string]string{"glm-5.2": "zai.glm-5"}, 1, "no twin"},
		{"twin on the same route", "deepseek-v4-pro:0813", map[string]string{"deepseek-v4-pro:0813": "glm-5.2"}, 1, "same route"},
		{"healthy twin", "deepseek-v4-pro:0813", map[string]string{"deepseek-v4-pro:0813": "zai.glm-5"}, 0, ""},
		{"no chat.model", "", map[string]string{"glm-5.2": "zai.glm-5"}, 0, ""},
	}
	for _, c := range cases {
		got := ModelFallbackFindings(c.chatModel, c.fallbacks, routeOf)
		require.Len(t, got, c.want, c.name)
		if c.contains != "" {
			assert.Contains(t, got[0], c.contains, c.name)
			assert.Contains(t, got[0], c.chatModel, c.name)
		}
	}
}

// The twin's response is marked so the dispatcher can tell the user.
func TestFallbackProvider_MarksTwinResponse_620(t *testing.T) {
	fb, _ := newFB("deepseek-v4-pro:0813",
		map[string]error{"deepseek-v4-pro:0813": infra429("session usage limit")},
		map[string]string{"deepseek-v4-pro:0813": "zai.glm-5"})
	resp, err := fb.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "deepseek-v4-pro:0813", resp.FallbackFrom)

	ok, _ := newFB("deepseek-v4-pro:0813", map[string]error{}, map[string]string{"deepseek-v4-pro:0813": "zai.glm-5"})
	resp, err = ok.Complete(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.FallbackFrom, "a primary answer is not marked")
}

// Through the real router: a 429 on the chat model fails over to its twin on
// the twin's own route (the highest level this package reaches).
func TestRouterWithFallback_429FailsOverAcrossProviders_620(t *testing.T) {
	ollama := &failStub{pinStub: pinStub{name: "ollama_cloud", model: "deepseek-v4-pro:0813"}, err: infra429("session usage limit")}
	bedrock := &pinStub{name: "bedrock"}
	r, err := NewRouter(ollama, []Route{{Prefix: "zai.", Provider: bedrock, Name: "bedrock"}},
		WithRouterFallbackName("ollama_cloud"), WithRouterDefaultModel("deepseek-v4-pro:0813"))
	require.NoError(t, err)
	fb := NewFallbackProvider(r, map[string]string{"deepseek-v4-pro:0813": "zai.glm-5"}, zerolog.Nop())
	resp, err := fb.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "bedrock", resp.ID)
	assert.Equal(t, "zai.glm-5", resp.Model)
	assert.Equal(t, "deepseek-v4-pro:0813", resp.FallbackFrom)
}

type failStub struct {
	pinStub
	err error
}

func (f *failStub) Complete(context.Context, []Message) (*ChatResponse, error) { return nil, f.err }
func (f *failStub) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return nil, f.err
}
func (f *failStub) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return nil, f.err
}
func (f *failStub) WithModel(m string) Provider {
	return &failStub{pinStub: pinStub{name: f.name, model: m}, err: f.err}
}

// Review 64a7 finding 4: chat.model on a route of the default's own kind is a
// no-op, served by the pinned default.
func TestRouter_ChatModelOnSameKindRouteIsUnchanged_620(t *testing.T) {
	def := (&pinStub{name: "bedrock-default"}).WithModel("zai.glm-5")
	route := &pinStub{name: "bedrock-route"}
	r, err := NewRouter(def, []Route{{Prefix: "zai.", Provider: route, Name: "bedrock"}},
		WithRouterFallbackName("bedrock"), WithRouterDefaultModel("zai.glm-5"))
	require.NoError(t, err)
	resp, err := r.Complete(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "bedrock-default", resp.ID)
	assert.Equal(t, "fallback", r.DefaultModelRoute())
}

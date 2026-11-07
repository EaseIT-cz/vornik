package api

import (
	"strings"
	"testing"
)

// GitHub #620 (nonswarm-llm-fallback design §11.2(b)). Found at: production,
// 2026-11-05: chat.model had no model_fallbacks entry and nothing said so.
func TestCheckChatModelFallback_620(t *testing.T) {
	routeOf := func(m string) string {
		if strings.HasPrefix(m, "zai.") {
			return "bedrock"
		}
		return "ollama_cloud"
	}
	cases := []struct {
		name      string
		routeOf   func(string) string
		model     string
		fallbacks map[string]string
		status    string
		contains  string
	}{
		{"no router", nil, "deepseek-v4-pro:0813", nil, "SKIPPED", "not the router"},
		{"empty map states it", routeOf, "deepseek-v4-pro:0813", nil, "OK", "no chat.router.model_fallbacks"},
		{"missing twin", routeOf, "deepseek-v4-pro:0813", map[string]string{"glm-5.2": "zai.glm-5"}, "WARNING", "no twin"},
		{"same-route twin", routeOf, "deepseek-v4-pro:0813", map[string]string{"deepseek-v4-pro:0813": "glm-5.2"}, "WARNING", "same route"},
		{"healthy twin", routeOf, "deepseek-v4-pro:0813", map[string]string{"deepseek-v4-pro:0813": "zai.glm-5"}, "OK", "fails over to zai.glm-5"},
	}
	for _, c := range cases {
		h := &DoctorHandlers{chatRouteOf: c.routeOf, chatModel: c.model, chatModelFallbacks: c.fallbacks}
		got := h.checkChatModelFallback()
		if got.Name != "chat_model_fallback" || got.Status != c.status || !strings.Contains(got.Message, c.contains) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, got.Status, got.Message, c.status, c.contains)
		}
	}
}

package api

import (
	"fmt"

	"vornik.io/vornik/internal/chat"
)

// checkChatModelFallback reports a chat.model with no usable twin in
// chat.router.model_fallbacks: on 2026-11-05 the dispatcher's model had none,
// so an Ollama quota 429 failed every chat turn although Bedrock was healthy
// (nonswarm-llm-fallback design §11.2(b), GitHub #620). The findings come from
// the same function the startup log uses.
func (h *DoctorHandlers) checkChatModelFallback() DoctorCheck {
	name := "chat_model_fallback"
	if h.chatRouteOf == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "the chat provider is not the router, so there is no fallback to check; skipping"}
	}
	if h.chatModel == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "chat.model is not set; each sub-provider uses its own model"}
	}
	if len(h.chatModelFallbacks) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("no chat.router.model_fallbacks configured: a 429 or outage on %s fails chat turns until it recovers", h.chatModel)}
	}
	if findings := chat.ModelFallbackFindings(h.chatModel, h.chatModelFallbacks, h.chatRouteOf); len(findings) > 0 {
		return DoctorCheck{Name: name, Status: "WARNING", Message: findings[0], Items: findings}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("chat.model %s fails over to %s on another route", h.chatModel, h.chatModelFallbacks[h.chatModel])}
}

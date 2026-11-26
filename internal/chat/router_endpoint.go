package chat

import (
	"fmt"

	"vornik.io/vornik/internal/config"
)

// RouterSubEndpoint is the endpoint URL a router sub-provider of kind calls,
// resolved from config the way the daemon's initChatRouter builds it, or
// cli=true for a subprocess-backed kind, which has no endpoint Vornik can see.
// ok is false for an unknown kind. Read by the daemon (the router itself and
// the agent model classifier, agent-administered design §18.6 item 2) and by
// the Art 15 report's recipients (GDPR design §4.22, #470). Moved here from
// internal/service so the CLI resolves it in the same place.
func RouterSubEndpoint(cfg config.ChatConfig, kind string) (endpoint string, cli, ok bool) {
	rcfg := cfg.Router
	switch kind {
	case "http":
		if rcfg.HTTP.Endpoint != "" {
			return rcfg.HTTP.Endpoint, false, true
		}
		return cfg.Endpoint, false, true
	case "vertex":
		if rcfg.Vertex.Endpoint != "" {
			return rcfg.Vertex.Endpoint, false, true
		}
		return VertexEndpoint(rcfg.Vertex.ProjectID, rcfg.Vertex.Location), false, true
	case "openrouter":
		return OpenRouterEndpoint(rcfg.OpenRouter.Endpoint), false, true
	case "ollama_cloud":
		return OllamaCloudEndpoint(rcfg.OllamaCloud.Endpoint), false, true
	case "bedrock":
		if rcfg.Bedrock.Region == "" {
			// No region, no endpoint: not offered (review 20261003-2ed0 B1).
			return "", false, false
		}
		return BedrockRuntimeEndpoint(rcfg.Bedrock.Region), false, true
	case "claude-subscription":
		return ClaudeSubscriptionEndpoint(), false, true
	case "codex-subscription":
		return CodexSubscriptionEndpoint(), false, true
	case "claude-cli", "codex-cli":
		return "", true, true
	}
	return "", false, false
}

// BedrockRuntimeEndpoint is the Bedrock runtime URL of an AWS region.
func BedrockRuntimeEndpoint(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// VertexEndpoint returns the OpenAI-compat Vertex AI URL for a given
// project + location. The trailing `/chat/completions` is stripped inside
// NewClient (via normalizeEndpoint), so only the base prefix is built.
//
// Location hostname rules:
//   - "" or "global" → `aiplatform.googleapis.com` (the location path
//     segment is still "global" — the global endpoint is not regionless).
//   - anything else  → `<location>-aiplatform.googleapis.com` (regional).
//
// Operators who point at an internal proxy or preview endpoint set
// chat.router.vertex.endpoint directly and skip this helper.
func VertexEndpoint(projectID, location string) string {
	if location == "" {
		location = "global"
	}
	host := "aiplatform.googleapis.com"
	if location != "global" {
		host = location + "-aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/endpoints/openapi",
		host, projectID, location)
}

// DefaultOpenRouterEndpoint is OpenRouter's OpenAI-compatibility base URL.
// normalizeEndpoint (inside NewClient) tolerates the trailing path.
const DefaultOpenRouterEndpoint = "https://openrouter.ai/api/v1"

// OpenRouterEndpoint returns the operator's endpoint override or the
// baked-in OpenRouter default when unset.
func OpenRouterEndpoint(endpoint string) string {
	if endpoint == "" {
		return DefaultOpenRouterEndpoint
	}
	return endpoint
}

// DefaultOllamaCloudEndpoint is Ollama Cloud's OpenAI-compatibility base URL.
// normalizeEndpoint (inside NewClient) tolerates the trailing path.
const DefaultOllamaCloudEndpoint = "https://ollama.com/v1"

// OllamaCloudEndpoint returns the operator's endpoint override or the
// baked-in Ollama Cloud default when unset.
func OllamaCloudEndpoint(endpoint string) string {
	if endpoint == "" {
		return DefaultOllamaCloudEndpoint
	}
	return endpoint
}

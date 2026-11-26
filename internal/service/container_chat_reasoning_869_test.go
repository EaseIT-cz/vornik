package service

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #869 (design B4): the chat router hands each OpenAI-compatible
// client the per-model template variable from model_limits; a model without
// one is absent, and a config with none yields nil.
//
// Found at: production (design review). Regression test at: unit. Lower
// because: the mapping is decided here.
func TestReasoningTemplateVariables_869(t *testing.T) {
	if got := reasoningTemplateVariables(nil); got != nil {
		t.Fatalf("nil config = %v", got)
	}
	cfg := &config.Config{}
	cfg.Runtime.AgentLLM.ModelLimits = map[string]config.ModelLimitConfig{
		"vllm/gemma4-26b": {ReasoningTemplateVariable: "enable_thinking"},
		"glm-5.3-flash":   {Reasoning: "low"},
	}
	want := map[string]string{"vllm/gemma4-26b": "enable_thinking"}
	if got := reasoningTemplateVariables(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

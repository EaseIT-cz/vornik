package executor

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #869 (reasoning control design B2): the agent's reasoning level is
// the role's, else the effective model's model_limits entry, else nothing.
// Before the change no level was resolved at all, so every model reasoned at
// its own default and the companion reviewer spent its cap thinking.
//
// Found at: production (companion reviews failed on the output cap).
// Regression test at: unit. Lower because: the level is resolved here; the
// agent and proxy have their own tests.
func TestReasoningLevelFor_869(t *testing.T) {
	limits := map[string]ModelLimit{"glm-5.3-flash": {Reasoning: "low"}, "other": {}}
	for _, tc := range []struct {
		name, model, role, want string
	}{
		{"model default", "glm-5.3-flash", "", "low"},
		{"role beats model", "glm-5.3-flash", "off", "off"},
		{"role alone", "other", "high", "high"},
		{"nothing set", "other", "", ""},
		{"unknown model", "nope", "", ""},
	} {
		role := &registry.SwarmRole{Reasoning: tc.role}
		if got := reasoningLevelFor(tc.model, limits, role); got != tc.want {
			t.Errorf("%s: level = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestApplyTokenLimits_InjectsReasoning_869(t *testing.T) {
	e := &Executor{config: &Config{ModelLimits: map[string]ModelLimit{"glm-5.3-flash": {Reasoning: "off"}}}}
	env := map[string]string{"VORNIK_LLM_MODEL": "glm-5.3-flash"}
	e.applyTokenLimits(env, env["VORNIK_LLM_MODEL"], &persistence.Task{}, &registry.SwarmRole{})
	if env["VORNIK_LLM_REASONING"] != "off" {
		t.Fatalf("VORNIK_LLM_REASONING = %q, want off", env["VORNIK_LLM_REASONING"])
	}
	env = map[string]string{"VORNIK_LLM_MODEL": "unlisted"}
	e.applyTokenLimits(env, env["VORNIK_LLM_MODEL"], &persistence.Task{}, &registry.SwarmRole{})
	if v, ok := env["VORNIK_LLM_REASONING"]; ok {
		t.Fatalf("an unset level must not be injected, got %q", v)
	}
}

// The warm path passes a model its roleEnv does not carry (a global default);
// the level must follow the model passed, not the env (slice 1 code review).
func TestApplyTokenLimits_FollowsThePassedModel_869(t *testing.T) {
	e := &Executor{config: &Config{ModelLimits: map[string]ModelLimit{"global-model": {Reasoning: "low", MaxTokens: 4096}}}}
	env := map[string]string{} // warm roleEnv: no VORNIK_LLM_MODEL
	e.applyTokenLimits(env, "global-model", &persistence.Task{}, &registry.SwarmRole{})
	if env["VORNIK_LLM_REASONING"] != "low" || env["VORNIK_LLM_MAX_TOKENS"] != "4096" {
		t.Fatalf("env = %v, want reasoning low and max tokens 4096 from the passed model", env)
	}
}

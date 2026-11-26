package service

// GitHub #891 (config-assistant design §7.3a): when the config assistant is
// wired as the healing producer with a judge it cannot use, start-up says so
// once, naming the setting. Found at: e2e-lane, the #809 bench.

import (
	"strings"
	"testing"
)

func TestHealingJudgeWarning_891(t *testing.T) {
	if got := healingJudgeWarning(""); got != "" {
		t.Fatalf("a usable pair warns nothing, got %q", got)
	}
	got := healingJudgeWarning(`assistant "vllm/gemma4-26b" and judge "vllm/gemma4-26b" are the same family (google)`)
	for _, want := range []string{"vllm/gemma4-26b", "config_assistant.judge_model", "recipe"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q should name %q", got, want)
		}
	}
}

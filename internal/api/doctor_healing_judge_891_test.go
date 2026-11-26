package api

// GitHub #891 (config-assistant design §7.3a): healing calls the config
// assistant without the feature's family check, so a one-model install
// refused every assistant candidate with nothing but a log line to say why.
// Found at: e2e-lane, the #809 bench (vllm/gemma4-26b paired with itself);
// tested at the doctor row, where an operator reads it.

import (
	"strings"
	"testing"
)

func TestCheckHealingJudgeFamily_891(t *testing.T) {
	t.Run("no healing assistant wired", func(t *testing.T) {
		h := &DoctorHandlers{}
		if got := h.checkHealingJudgeFamily(); got.Status != "SKIPPED" || got.Name != "healing_judge_family" {
			t.Fatalf("got %+v, want SKIPPED: not evaluated is never OK", got)
		}
		h.SetHealingJudge(func() (bool, string) { return false, "" })
		if got := h.checkHealingJudgeFamily(); got.Status != "SKIPPED" {
			t.Fatalf("got %+v, want SKIPPED", got)
		}
	})
	t.Run("usable pair", func(t *testing.T) {
		h := &DoctorHandlers{}
		h.SetHealingJudge(func() (bool, string) { return true, "" })
		if got := h.checkHealingJudgeFamily(); got.Status != "OK" {
			t.Fatalf("got %+v, want OK", got)
		}
	})
	t.Run("the bench's pair", func(t *testing.T) {
		h := &DoctorHandlers{}
		reason := `assistant "vllm/gemma4-26b" and judge "vllm/gemma4-26b" are the same family (google)`
		h.SetHealingJudge(func() (bool, string) { return true, reason })
		got := h.checkHealingJudgeFamily()
		if got.Status != "WARNING" {
			t.Fatalf("status = %q, want WARNING", got.Status)
		}
		for _, want := range []string{"vllm/gemma4-26b", "same family"} {
			if !strings.Contains(got.Message, want) {
				t.Errorf("message %q should name %q", got.Message, want)
			}
		}
		for _, want := range []string{"config_assistant.judge_model", "recipe"} {
			if !strings.Contains(got.Message, want) {
				t.Errorf("message %q should give the remedy, naming %q", got.Message, want)
			}
		}
	})
}

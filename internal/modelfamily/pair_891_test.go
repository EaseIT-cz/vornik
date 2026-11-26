package modelfamily

// GitHub #891 (config-assistant design §7.3a): one rule for "is this judge
// usable against this assistant". Found at: e2e-lane, the #809 bench, where
// vllm/gemma4-26b was paired with itself and every healing candidate was
// refused while nothing but a log line said why.

import (
	"strings"
	"testing"
)

func TestPairProblem_891(t *testing.T) {
	cases := []struct {
		name, assistant, judge string
		ok, unclassifiable     bool
		mentions               []string
	}{
		{name: "different families", assistant: "vllm/gemma4-26b", judge: "deepseek-v4-pro:0813", ok: true},
		{name: "same model", assistant: "vllm/gemma4-26b", judge: "vllm/gemma4-26b", mentions: []string{"vllm/gemma4-26b", "same family"}},
		{name: "same family", assistant: "glm-5.2:cloud", judge: "glm-4.5-air", mentions: []string{"glm-5.2:cloud", "glm-4.5-air", "same family"}},
		{name: "unclassifiable judge", assistant: "glm-5.2", judge: "mystery-model", unclassifiable: true, mentions: []string{"mystery-model", "cannot classify"}},
		{name: "empty judge", assistant: "glm-5.2", judge: "", mentions: []string{"judge model is not set"}},
		{name: "empty assistant", assistant: " ", judge: "glm-5.2", mentions: []string{"assistant model is not set"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, unclassifiable := PairProblem(c.assistant, c.judge)
			if c.ok != (reason == "") {
				t.Fatalf("PairProblem(%q, %q) = %q, want ok=%v", c.assistant, c.judge, reason, c.ok)
			}
			if unclassifiable != c.unclassifiable {
				t.Errorf("unclassifiable = %v, want %v", unclassifiable, c.unclassifiable)
			}
			for _, m := range c.mentions {
				if !strings.Contains(reason, m) {
					t.Errorf("reason %q should mention %q", reason, m)
				}
			}
		})
	}
}

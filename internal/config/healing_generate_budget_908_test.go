package config

import (
	"testing"
	"time"
)

// GitHub #908: the healing generate budget is the assistant's own loop budget
// (config_assistant.request_timeout) plus a margin for the judge call and the
// ledger writes after the loop. Every layer above the producer derives its
// deadline from this one value, so they nest. Found at production (bench arm C
// exploratory run: 12 of 12 generate calls cut at 60 s by the handler).
func TestHealingGenerateBudget_908(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   string
		want time.Duration
	}{
		{"default request_timeout", "", 7 * time.Minute},
		{"shipped default", ConfigAssistantDefaultRequestTimeout, 7 * time.Minute},
		{"operator raised it", "10m", 12 * time.Minute},
		{"short, as in tests", "100ms", 2*time.Minute + 100*time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssistantConfig{RequestTimeout: tc.rt}.HealingGenerateBudget()
			if got != tc.want {
				t.Fatalf("HealingGenerateBudget(%q) = %s, want %s", tc.rt, got, tc.want)
			}
			if got <= 60*time.Second {
				t.Fatalf("the budget %s does not outlive the 60 s that cut every bench generation", got)
			}
		})
	}
	if DefaultHealingGenerateBudget != 7*time.Minute {
		t.Fatalf("DefaultHealingGenerateBudget = %s, want 7m", DefaultHealingGenerateBudget)
	}
}

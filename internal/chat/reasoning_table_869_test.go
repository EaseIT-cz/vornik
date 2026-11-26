package chat

import (
	"strings"
	"testing"
)

// GitHub #869, reasoning control design B4: every sub-provider kind declares
// what it does with each reasoning level, and a kind with no declaration
// cannot be registered. So a provider added later must say how it maps off,
// low, medium and high (or that it cannot), and an operator's level is never
// silently lost on a route nobody mapped.
//
// Found at: production (companion reviews failed on the output cap; no route
// mapped a reasoning level). Regression test at: unit. Lower because: the
// table and the registration are decided here.

var reasoningLevels = []string{ReasoningEffortOff, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}

// The nine kinds the daemon registers today (container_chat.go, 2026-11-20).
var registeredKinds = []string{
	"http", "openrouter", "ollama_cloud", "vertex", "bedrock",
	"claude-subscription", "codex-subscription", "claude-cli", "codex-cli",
}

func TestReasoningTable_EveryKindMapsEveryLevel_869(t *testing.T) {
	examined := 0
	for _, kind := range registeredKinds {
		for _, level := range reasoningLevels {
			got, ok := ReasoningSupportFor(kind, level)
			if !ok {
				t.Errorf("kind %q declares nothing for level %q", kind, level)
				continue
			}
			switch got {
			case ReasoningSent, ReasoningApproximated, ReasoningUnsupported:
			default:
				t.Errorf("kind %q level %q: %q is not an answer", kind, level, got)
			}
			examined++
		}
	}
	t.Logf("reasoning table: %d kinds x %d levels examined (%d)", len(registeredKinds), len(reasoningLevels), examined)
	if examined != len(registeredKinds)*len(reasoningLevels) {
		t.Fatalf("examined %d, want %d", examined, len(registeredKinds)*len(reasoningLevels))
	}
}

func TestSubProviders_AddRefusesAnUnmappedKind_869(t *testing.T) {
	subs := NewSubProviders()
	if err := subs.Add("http", nil); err != nil {
		t.Fatalf("a mapped kind must register: %v", err)
	}
	err := subs.Add("brand-new-provider", nil)
	if err == nil || !strings.Contains(err.Error(), "brand-new-provider") {
		t.Fatalf("an unmapped kind must be refused, naming it; got %v", err)
	}
	m := subs.Map()
	if _, ok := m["http"]; !ok || len(m) != 1 {
		t.Fatalf("Map = %v, want only the registered kind", m)
	}
	m["injected"] = nil
	if _, ok := subs.Map()["injected"]; ok {
		t.Fatal("Map must return a copy: writing to it registered a provider")
	}
}

// The OpenAI-compatible family and Bedrock send every level (slice 1); the
// table must say so, so the counter reports what the wire does.
func TestReasoningTable_SentWhereSliceOneSends_869(t *testing.T) {
	for _, kind := range []string{"http", "openrouter", "ollama_cloud", "vertex", "bedrock"} {
		for _, level := range reasoningLevels {
			if got, _ := ReasoningSupportFor(kind, level); got != ReasoningSent {
				t.Errorf("%s/%s = %q, want sent", kind, level, got)
			}
		}
	}
	if got, _ := ReasoningSupportFor("codex-cli", ReasoningEffortLow); got != ReasoningUnsupported {
		t.Errorf("codex-cli ignores the effort flag (design A2); got %q", got)
	}
}

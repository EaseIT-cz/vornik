package service

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// The optional-work switch (breaker design §5.3d, 2026-09-26) wraps the chat
// client outside the logging provider when set, and not at all when unset.
func TestWrapOptionalWork(t *testing.T) {
	inner := chat.NewLoggingProvider(&identifiableStubProvider{id: "base"}, zerolog.Nop())
	if got := wrapOptionalWork(inner, config.ChatOptionalWorkConfig{}, zerolog.Nop()); got != inner {
		t.Fatalf("an unset switch must leave the chain untouched, got %T", got)
	}
	got := wrapOptionalWork(inner, config.ChatOptionalWorkConfig{DisabledModels: []string{"slow"}}, zerolog.Nop())
	gate, ok := got.(*chat.OptionalWorkGate)
	if !ok {
		t.Fatalf("a set switch wraps the client in the gate, got %T", got)
	}
	if gate.Unwrap() != inner {
		t.Fatal("the gate must sit directly outside the logging provider")
	}
}

// The reranker's boot line reports its RESOLVED state; a model whose optional
// work is refused makes it inert (reference architecture, "the reranker
// trap"; breaker design §5.3d).
func TestOptionalWorkDisables(t *testing.T) {
	if optionalWorkDisables(config.ChatOptionalWorkConfig{}, "m") {
		t.Fatal("an unset switch disables nothing")
	}
	if !optionalWorkDisables(config.ChatOptionalWorkConfig{Disabled: true}, "m") {
		t.Fatal("disabled: true covers every model")
	}
	if !optionalWorkDisables(config.ChatOptionalWorkConfig{DisabledModels: []string{"m"}}, "m") ||
		optionalWorkDisables(config.ChatOptionalWorkConfig{DisabledModels: []string{"m"}}, "other") {
		t.Fatal("disabled_models is an exact list")
	}
}

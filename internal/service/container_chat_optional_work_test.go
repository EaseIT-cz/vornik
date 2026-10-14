package service

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/observability"
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

// Issue #308 (2026-10-08): the container builds the gate with
// NewOptionalWorkGate(..., nil), so the claim was that
// vornik_chat_optional_work_refused_total never counts in production. The
// metrics arrive afterwards, through wireComponentMetrics' SetMetrics on the
// finished client; this drives that real boot path end to end: the gate as
// initChat leaves it, the registry /metrics serves, then a refused best-effort
// call, plain and model-pinned, must count with its {model, call_site} labels.
func TestOptionalWorkGate_RefusalCountsOnServedRegistry(t *testing.T) {
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	inner := chat.NewLoggingProvider(&identifiableStubProvider{id: "base", model: "slow"}, zerolog.Nop())
	c := &Container{Observability: obs}
	c.ChatClient = wrapOptionalWork(inner, config.ChatOptionalWorkConfig{DisabledModels: []string{"slow", "pinned"}}, zerolog.Nop())

	c.wireComponentMetrics()

	ctx := chat.WithCallSite(chat.WithBestEffort(context.Background()), "narrator")
	if _, err := c.ChatClient.Complete(ctx, nil); !errors.Is(err, chat.ErrOptionalWorkDisabled) {
		t.Fatalf("best-effort call to a disabled model: err = %v, want ErrOptionalWorkDisabled", err)
	}
	pinned := c.ChatClient.(chat.ModelOverridable).WithModel("pinned")
	if _, err := pinned.Complete(chat.WithCallSite(chat.WithBestEffort(context.Background()), "reranker"), nil); !errors.Is(err, chat.ErrOptionalWorkDisabled) {
		t.Fatalf("best-effort call to a disabled pin: err = %v, want ErrOptionalWorkDisabled", err)
	}

	want := map[string]float64{"slow/narrator": 1, "pinned/reranker": 1}
	got := map[string]float64{}
	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "vornik_chat_optional_work_refused_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			got[labels["model"]+"/"+labels["call_site"]] = m.GetCounter().GetValue()
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("optional_work_refused_total{%s} = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Optional work can be switched off per model (breaker design §5.3d,
// 2026-09-26). Incident: the slow-hardware bench arm, where narration, titles,
// classification and consolidation queued requests on the one slow GPU beside
// the task traffic; the operator asked for non-essential features to be
// switchable off on slow backends.

func TestOptionalWorkGate_EmptyConfigWrapsNothing(t *testing.T) {
	inner := &fakeInner{model: "m"}
	if got := NewOptionalWorkGate(inner, OptionalWorkConfig{}, nil); got != Provider(inner) {
		t.Fatalf("an empty config must return inner unchanged, got %T", got)
	}
}

func TestOptionalWorkGate_RefusesOnlyMarkedCallsToDisabledModels(t *testing.T) {
	inner := &fakeInner{model: "slow"}
	m := NewMetrics(prometheus.NewRegistry())
	g := NewOptionalWorkGate(inner, OptionalWorkConfig{DisabledModels: []string{"slow"}}, m)
	be := WithCallSite(WithBestEffort(context.Background()), "narrator.line")

	for name, call := range map[string]func(context.Context) error{
		"Complete":                func(ctx context.Context) error { _, err := g.Complete(ctx, nil); return err },
		"CompleteWithTools":       func(ctx context.Context) error { _, err := g.CompleteWithTools(ctx, nil, nil); return err },
		"CompleteWithToolsStream": func(ctx context.Context) error { _, err := g.CompleteWithToolsStream(ctx, nil, nil, nil); return err },
	} {
		before := inner.calls.Load()
		if err := call(be); !errors.Is(err, ErrOptionalWorkDisabled) {
			t.Fatalf("%s: a best-effort call to a disabled model is refused, got %v", name, err)
		}
		if inner.calls.Load() != before {
			t.Fatalf("%s: the refusal must not reach the model", name)
		}
		if err := call(context.Background()); err != nil {
			t.Fatalf("%s: an unmarked call is task traffic and always passes: %v", name, err)
		}
		if inner.calls.Load() != before+1 {
			t.Fatalf("%s: the unmarked call must reach the model", name)
		}
	}
	if got := testutil.ToFloat64(m.OptionalWorkRefused.WithLabelValues("slow", "narrator.line")); got != 3 {
		t.Fatalf("refused counter = %v, want 3", got)
	}
}

func TestOptionalWorkGate_JudgesTheResolvedModel(t *testing.T) {
	inner := &fakeInner{model: "fast"}
	g := NewOptionalWorkGate(inner, OptionalWorkConfig{DisabledModels: []string{"slow"}}, nil)
	be := WithBestEffort(context.Background())
	if _, err := g.Complete(be, nil); err != nil {
		t.Fatalf("a model not in the list passes: %v", err)
	}
	pinned := g.(ModelOverridable).WithModel("slow")
	if _, err := pinned.Complete(be, nil); !errors.Is(err, ErrOptionalWorkDisabled) {
		t.Fatalf("a WithModel pin is judged by the pinned model, got %v", err)
	}
}

func TestOptionalWorkGate_DisabledRefusesAnyModel(t *testing.T) {
	g := NewOptionalWorkGate(&fakeInner{model: "anything"}, OptionalWorkConfig{Disabled: true}, nil)
	if _, err := g.Complete(WithBestEffort(context.Background()), nil); !errors.Is(err, ErrOptionalWorkDisabled) {
		t.Fatalf("disabled: true refuses every best-effort call, got %v", err)
	}
}

func TestOptionalWorkGate_ForwardsTheDecoratorInterfaces(t *testing.T) {
	inner := &fakeInner{model: "m"}
	g := NewOptionalWorkGate(inner, OptionalWorkConfig{Disabled: true}, nil)
	if u, ok := g.(Unwrapper); !ok || u.Unwrap() != Provider(inner) {
		t.Fatal("the gate must unwrap to inner, so capability lookup sees through it")
	}
	for name, ok := range map[string]bool{
		"Pinger":              implements[Pinger](g),
		"ModelHealthReporter": implements[ModelHealthReporter](g),
		"ModelLister":         implements[ModelLister](g),
		"ModelOverridable":    implements[ModelOverridable](g),
	} {
		if !ok {
			t.Errorf("the gate sits above the logging provider and must forward %s as it does", name)
		}
	}
	if g.Model() != "m" {
		t.Fatalf("Model() = %q", g.Model())
	}
}

func implements[T any](p Provider) bool { _, ok := p.(T); return ok }

// The daemon attaches metrics after wiring (container.go SetMetrics on the
// finished client), so the counter must work through SetMetrics alone.
func TestOptionalWorkGate_MetricsArriveThroughSetMetrics(t *testing.T) {
	g := NewOptionalWorkGate(&fakeInner{model: "slow"}, OptionalWorkConfig{DisabledModels: []string{"slow"}}, nil)
	m := NewMetrics(prometheus.NewRegistry())
	g.SetMetrics(m)
	_, _ = g.Complete(WithCallSite(WithBestEffort(context.Background()), "memory.titler"), nil)
	if got := testutil.ToFloat64(m.OptionalWorkRefused.WithLabelValues("slow", "memory.titler")); got != 1 {
		t.Fatalf("refused counter after SetMetrics = %v, want 1", got)
	}
}

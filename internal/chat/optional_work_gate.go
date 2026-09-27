package chat

import (
	"context"
	"errors"
)

// ErrOptionalWorkDisabled is returned, without a network call, for a
// best-effort call (WithBestEffort) to a model whose optional work the
// operator switched off (breaker design §5.3d). Callers treat it as a state,
// not a failure: they degrade exactly as they would without an answer, and
// do not log it as a warning.
var ErrOptionalWorkDisabled = errors.New("optional LLM work is disabled for this model")

// OptionalWorkConfig selects the models whose optional work is refused.
type OptionalWorkConfig struct {
	// Disabled refuses every best-effort call, whatever the model.
	Disabled bool
	// DisabledModels refuses best-effort calls to these exact model ids.
	DisabledModels []string
}

// Empty reports a config that refuses nothing.
func (c OptionalWorkConfig) Empty() bool { return !c.Disabled && len(c.DisabledModels) == 0 }

// OptionalWorkGate refuses best-effort calls to disabled models. It wraps the
// daemon's chat client OUTSIDE the logging provider, so a refusal makes no
// request, writes no "llm call" line and is no breaker sample. Unmarked calls
// (task traffic) always pass.
type OptionalWorkGate struct {
	inner    Provider
	disabled bool
	models   map[string]struct{}
	metrics  *Metrics
}

// NewOptionalWorkGate wraps inner, or returns inner unchanged when cfg
// refuses nothing, so an unset deployment's chain is exactly today's.
func NewOptionalWorkGate(inner Provider, cfg OptionalWorkConfig, m *Metrics) Provider {
	if inner == nil || cfg.Empty() {
		return inner
	}
	models := make(map[string]struct{}, len(cfg.DisabledModels))
	for _, id := range cfg.DisabledModels {
		models[id] = struct{}{}
	}
	return &OptionalWorkGate{inner: inner, disabled: cfg.Disabled, models: models, metrics: m}
}

// refused reports whether this call is optional work the operator disabled
// for the model that will serve it, counting the refusal.
func (g *OptionalWorkGate) refused(ctx context.Context) bool {
	if !IsBestEffort(ctx) {
		return false
	}
	model := g.inner.Model()
	if _, listed := g.models[model]; !g.disabled && !listed {
		return false
	}
	if g.metrics != nil && g.metrics.OptionalWorkRefused != nil {
		g.metrics.OptionalWorkRefused.WithLabelValues(model, CallSiteFromContext(ctx)).Inc()
	}
	return true
}

// Complete implements Provider.
func (g *OptionalWorkGate) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	if g.refused(ctx) {
		return nil, ErrOptionalWorkDisabled
	}
	return g.inner.Complete(ctx, messages)
}

// CompleteWithTools implements Provider.
func (g *OptionalWorkGate) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	if g.refused(ctx) {
		return nil, ErrOptionalWorkDisabled
	}
	return g.inner.CompleteWithTools(ctx, messages, tools)
}

// CompleteWithToolsStream implements Provider.
func (g *OptionalWorkGate) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	if g.refused(ctx) {
		return nil, ErrOptionalWorkDisabled
	}
	return g.inner.CompleteWithToolsStream(ctx, messages, tools, onText)
}

// Model delegates.
func (g *OptionalWorkGate) Model() string { return g.inner.Model() }

// SetMetrics keeps m for the refusal counter and forwards it down the chain.
// The daemon attaches chat metrics to the finished client after wiring, so
// this, not the constructor, is where production metrics arrive.
func (g *OptionalWorkGate) SetMetrics(m *Metrics) {
	g.metrics = m
	g.inner.SetMetrics(m)
}

// WithModel keeps the gate around a model-pinned clone, so a pin is judged by
// the pinned model.
func (g *OptionalWorkGate) WithModel(model string) Provider {
	o, ok := g.inner.(ModelOverridable)
	if !ok {
		return g
	}
	return &OptionalWorkGate{inner: o.WithModel(model), disabled: g.disabled, models: g.models, metrics: g.metrics}
}

// The rest forwards what the logging provider forwards, because the gate now
// sits above it and callers assert these on the top of the chain.

// Unwrap implements Unwrapper.
func (g *OptionalWorkGate) Unwrap() Provider { return g.inner }

// ModelHealthSnapshot forwards the breaker read for the doctor.
func (g *OptionalWorkGate) ModelHealthSnapshot() []ModelHealthSnapshot {
	if r, ok := g.inner.(ModelHealthReporter); ok {
		return r.ModelHealthSnapshot()
	}
	return nil
}

// ListModels forwards model discovery.
func (g *OptionalWorkGate) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if l, ok := g.inner.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	return nil, nil
}

// Ping forwards the readiness probe.
func (g *OptionalWorkGate) Ping(ctx context.Context) error {
	if p, ok := g.inner.(Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ListModelsAggregated forwards the per-sub-provider model breakdown.
func (g *OptionalWorkGate) ListModelsAggregated(ctx context.Context) (ListModelsResult, bool) {
	if agg, ok := g.inner.(interface {
		ListModelsAggregated(context.Context) (ListModelsResult, bool)
	}); ok {
		return agg.ListModelsAggregated(ctx)
	}
	return ListModelsResult{}, false
}

var (
	_ Provider         = (*OptionalWorkGate)(nil)
	_ ModelOverridable = (*OptionalWorkGate)(nil)
)

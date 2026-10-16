package autonomy

// GitHub #142 item 2 (2026-10-14, outcome-quality-design §S8 A1/F6): an
// autonomy evaluation whose model call timed out was filed as LLM_ERROR with
// the bare "context deadline exceeded", the same reason as a model that
// answered with an error, though the remedies differ (raise a bound or use a
// faster model, vs check the model's health). The reason also names the bound
// that actually fired (review e0bf #2).

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestEvaluate_ATimedOutModelCallIsLLMTimeoutAndNamesTheRemedy(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	metrics := NewMetrics(prometheus.NewRegistry())
	// The provider's own per-call deadline, wrapped the way the chat client
	// wraps it (%w since §S2 C4), under a still-live evaluation context.
	client := &fakeChatProvider{err: fmt.Errorf("chat request failed: %w", context.DeadlineExceeded)}
	m := New(client, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evalRepo),
		WithMetrics(metrics),
	)

	err := m.evaluate(context.Background(), llmEvalProject())
	require.Error(t, err)

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeLLMTimeout, entries[0].Outcome,
		"a timed-out evaluation was filed as LLM_ERROR, the engine-error reason")
	assert.Contains(t, entries[0].Reason, "chat.timeout",
		"the provider's per-call timeout fired: the reason must name it")
	assert.NotContains(t, entries[0].Reason, "evaluate_timeout",
		"the evaluate timeout did not fire and must not be blamed")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ErrorsTotal.WithLabelValues("family")),
		"the shared error counter still counts it")
}

// blockingChatProvider answers only when the call's context ends.
type blockingChatProvider struct{ fakeChatProvider }

func (b *blockingChatProvider) CompleteWithTools(ctx context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEvaluate_TheEvaluateTimeoutRunningOutIsNamedAsSuch(t *testing.T) {
	evalRepo := &captureEvalRepo{}
	m := New(&blockingChatProvider{}, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithEvaluationRepository(evalRepo),
		WithMetrics(NewMetrics(prometheus.NewRegistry())),
	)
	p := llmEvalProject()
	p.Autonomy.EvaluateTimeout = "50ms"

	require.Error(t, m.evaluate(context.Background(), p))

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeLLMTimeout, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "evaluate_timeout",
		"the evaluate timeout ran out: the reason must say which bound to raise")
}

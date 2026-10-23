package executor

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// GitHub #363 (permanent-failure design §8.8 D2): the executor hands the
// scheduler a retry hold once, only for a MODEL_UNHEALTHY failure, and an
// entry nobody collects is pruned.
func TestRetryHold_RecordAndCollect(t *testing.T) {
	mu := &chat.ModelUnhealthyError{Route: "agent", Model: "never-seen", State: "open"}

	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.recordRetryHold("t1", mu)
	_, ok := e.RetryNotBefore("t1")
	assert.False(t, ok, "without an agent breaker there is no hold")

	e = NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil,
		WithAgentHealth(newTestAgentHealthRegistry()))
	e.recordRetryHold("t2", errors.New("schema violation: missing keys"))
	_, ok = e.RetryNotBefore("t2")
	assert.False(t, ok, "a failure that is not MODEL_UNHEALTHY is not held")

	before := time.Now()
	e.recordRetryHold("t3", mu)
	at, ok := e.RetryNotBefore("t3")
	require.True(t, ok, "a MODEL_UNHEALTHY failure is held")
	assert.WithinDuration(t, before.Add(30*time.Second), at, 5*time.Second, "the bounded back-off is one cooldown")
	_, ok = e.RetryNotBefore("t3")
	assert.False(t, ok, "a hold is collected once")

	e.recordRetryHold("stale", mu)
	e.retryHold.mu.Lock()
	h := e.retryHold.at["stale"]
	h.stored = h.stored.Add(-retryHoldMaxAge - time.Second)
	e.retryHold.at["stale"] = h
	e.retryHold.mu.Unlock()
	e.recordRetryHold("fresh", mu)
	_, ok = e.RetryNotBefore("stale")
	assert.False(t, ok, "an entry nobody collected is pruned on the next store")
	_, ok = e.RetryNotBefore("fresh")
	assert.True(t, ok)
}

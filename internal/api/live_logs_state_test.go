package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// review-20261008-f680 finding 4 (MEDIUM), GitHub #92 (2026-10-08): the
// per-key maps were never evicted. A released stream or wait leaves no entry.
func TestLiveLogState_CountersEvictedOnRelease(t *testing.T) {
	st := newLiveLogState()
	rel, ok := st.acquireStream("k1")
	require.True(t, ok)
	relW, ok := st.acquireWaiter("k1")
	require.True(t, ok)
	rel()
	relW()
	st.mu.Lock()
	defer st.mu.Unlock()
	assert.Empty(t, st.streams)
	assert.Empty(t, st.waiters)
	assert.Zero(t, st.streamsTotal)
	assert.Zero(t, st.waitersTotal)
}

// review-20261008-f680 finding 4, GitHub #92 (2026-10-08): a global cap on
// concurrent agent streams and waits, across keys.
func TestLiveLogState_GlobalCapsRefuse(t *testing.T) {
	st := newLiveLogState()
	st.maxStreamsTotal = 1
	st.maxWaitersTotal = 1
	_, ok := st.acquireStream("k1")
	require.True(t, ok)
	_, ok = st.acquireStream("k2")
	assert.False(t, ok, "another key is refused at the global stream cap")
	_, ok = st.acquireWaiter("k1")
	require.True(t, ok)
	_, ok = st.acquireWaiter("k2")
	assert.False(t, ok, "another key is refused at the global wait cap")
}

// review-20261008-f680 finding 4, GitHub #92 (2026-10-08): a rate bucket idle
// long enough to be full again is dropped (it is equivalent to no bucket).
func TestLiveLogState_IdleBucketsExpire(t *testing.T) {
	st := newLiveLogState()
	now := time.Unix(1_700_000_000, 0)
	st.now = func() time.Time { return now }
	ok, _ := st.allow("k1")
	require.True(t, ok)
	now = now.Add(2 * time.Minute)
	ok, _ = st.allow("k2")
	require.True(t, ok)
	st.mu.Lock()
	defer st.mu.Unlock()
	_, has := st.buckets["k1"]
	assert.False(t, has, "k1's bucket refilled long ago and is evicted")
	assert.Len(t, st.buckets, 1)
}

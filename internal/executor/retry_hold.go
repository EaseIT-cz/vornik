package executor

import (
	"sync"
	"time"
)

// retryHolds carries, per task, the time before which a retry of a failed
// attempt cannot succeed because the model circuit that rejected it is still
// open (permanent-failure design §8.8 D2, GitHub #363). handleFailure writes
// it; the scheduler's taskFailed collects it through RetryNotBefore before
// it requeues the task. It lives only as long as the in-memory breaker it
// describes.
type retryHolds struct {
	mu sync.Mutex
	at map[string]retryHold
}

type retryHold struct {
	notBefore time.Time // on the breaker's clock
	stored    time.Time // on the wall clock, for pruning
}

// retryHoldMaxAge prunes an entry the scheduler never collected. A hold is
// at most one breaker cooldown long and is collected within one dispatch
// watch tick of being stored, so ten minutes is far past any real use.
const retryHoldMaxAge = 10 * time.Minute

// recordRetryHold stores the hold for a failed attempt that will retry, when
// err is a MODEL_UNHEALTHY reject (agenthealth.Registry.NextAdmission).
// Entries older than retryHoldMaxAge are pruned on every store, so one the
// scheduler never collects cannot accumulate.
func (e *Executor) recordRetryHold(taskID string, err error) {
	at, basis, ok := e.agentHealth.NextAdmission(err)
	if !ok {
		return
	}
	now := time.Now()
	e.retryHold.mu.Lock()
	if e.retryHold.at == nil {
		e.retryHold.at = make(map[string]retryHold)
	}
	for id, h := range e.retryHold.at {
		if now.Sub(h.stored) > retryHoldMaxAge {
			delete(e.retryHold.at, id)
		}
	}
	e.retryHold.at[taskID] = retryHold{notBefore: at, stored: now}
	e.retryHold.mu.Unlock()
	e.logger.Info().Str("task_id", taskID).Time("not_before", at).Str("basis", basis).
		Msg("retry hold: the failed attempt was rejected by an open model circuit; the retry waits until it can admit a call")
}

// RetryNotBefore returns and forgets the hold recorded for taskID. A miss is
// (zero, false): no hold. Implements scheduler.RetryHoldSource.
func (e *Executor) RetryNotBefore(taskID string) (time.Time, bool) {
	e.retryHold.mu.Lock()
	defer e.retryHold.mu.Unlock()
	h, ok := e.retryHold.at[taskID]
	delete(e.retryHold.at, taskID)
	return h.notBefore, ok
}

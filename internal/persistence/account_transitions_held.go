package persistence

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrTransitionRecoveryUnsupported is returned when the wrapped repository has no recovery.
var ErrTransitionRecoveryUnsupported = errors.New("persistence: this account-transition repository cannot list or recover rows")

// HeldTransitions wraps an AccountTransitionRepository and records the
// owners this process holds (design 22 §22.2.16b, GitHub #669 item 3): the
// recovery verb refuses a ticket held here. An owner enters before Begin
// runs, so no moment exists between Begin's commit and the entry, and leaves
// when Begin fails or after Finish or Abort succeed; a failed Finish keeps
// it, because the row is still busy under that owner.
type HeldTransitions struct {
	inner AccountTransitionRepository
	mu    sync.Mutex
	held  map[string]int
}

// TrackHeldTransitions wraps inner.
func TrackHeldTransitions(inner AccountTransitionRepository) *HeldTransitions {
	return &HeldTransitions{inner: inner, held: map[string]int{}}
}

var (
	_ AccountTransitionRepository = (*HeldTransitions)(nil)
	_ AccountTransitionRecovery   = (*HeldTransitions)(nil)
)

// Holds reports whether this process holds owner's ticket.
func (h *HeldTransitions) Holds(owner string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[owner] > 0
}

func (h *HeldTransitions) enter(owner string) {
	h.mu.Lock()
	h.held[owner]++
	h.mu.Unlock()
}

func (h *HeldTransitions) leave(owner string) {
	h.mu.Lock()
	if h.held[owner]--; h.held[owner] <= 0 {
		delete(h.held, owner)
	}
	h.mu.Unlock()
}

// ReadDecisionEpoch implements AccountTransitionRepository.
func (h *HeldTransitions) ReadDecisionEpoch(ctx context.Context, ns string) (AccountDecisionGuard, error) {
	return h.inner.ReadDecisionEpoch(ctx, ns)
}

// Begin implements AccountTransitionRepository.
func (h *HeldTransitions) Begin(ctx context.Context, scope, owner, operation string, now time.Time) (AccountTransitionTicket, error) {
	h.enter(owner)
	t, err := h.inner.Begin(ctx, scope, owner, operation, now)
	if err != nil {
		h.leave(owner)
	}
	return t, err
}

// BeginConfig implements AccountTransitionRepository.
func (h *HeldTransitions) BeginConfig(ctx context.Context, scope, owner, operation string, now time.Time, guard AccountDecisionGuard) (AccountTransitionTicket, error) {
	h.enter(owner)
	t, err := h.inner.BeginConfig(ctx, scope, owner, operation, now, guard)
	if err != nil {
		h.leave(owner)
	}
	return t, err
}

// Finish implements AccountTransitionRepository.
func (h *HeldTransitions) Finish(ctx context.Context, t AccountTransitionTicket, p AccountPublication) error {
	err := h.inner.Finish(ctx, t, p)
	if err == nil {
		h.leave(t.Owner)
	}
	return err
}

// Abort implements AccountTransitionRepository.
func (h *HeldTransitions) Abort(ctx context.Context, t AccountTransitionTicket) error {
	err := h.inner.Abort(ctx, t)
	if err == nil {
		h.leave(t.Owner)
	}
	return err
}

// ListBusy implements AccountTransitionRecovery over the wrapped repository.
func (h *HeldTransitions) ListBusy(ctx context.Context) ([]AccountTransitionRow, int, error) {
	r, ok := h.inner.(AccountTransitionRecovery)
	if !ok {
		return nil, 0, ErrTransitionRecoveryUnsupported
	}
	return r.ListBusy(ctx)
}

// Recover implements AccountTransitionRecovery over the wrapped repository.
// The held-ticket refusal is the caller's (the recovery service), which
// orders it before the config-write lock.
func (h *HeldTransitions) Recover(ctx context.Context, scope string, epoch int64, owner string) (AccountTransitionRow, error) {
	r, ok := h.inner.(AccountTransitionRecovery)
	if !ok {
		return AccountTransitionRow{}, ErrTransitionRecoveryUnsupported
	}
	return r.Recover(ctx, scope, epoch, owner)
}

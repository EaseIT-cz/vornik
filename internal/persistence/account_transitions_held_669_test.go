package persistence

// GitHub #669 item 3 (design 22, §22.2.16b): the in-memory set of tickets
// this process holds. An owner enters before Begin runs, leaves when Begin
// fails, and leaves only after Finish or Abort succeed, so a failed Finish
// keeps it. Found at: production (2026-10-28). Regression test at: unit
// (the set is process memory). Lower because the set has no database half.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubTransitions struct {
	beginErr, finishErr error
	sawHeld             bool
	held                *HeldTransitions
}

func (s *stubTransitions) ReadDecisionEpoch(context.Context, string) (AccountDecisionGuard, error) {
	return nil, nil
}
func (s *stubTransitions) Begin(_ context.Context, scope, owner, _ string, _ time.Time) (AccountTransitionTicket, error) {
	s.sawHeld = s.held.Holds(owner)
	return AccountTransitionTicket{Scope: scope, Epoch: 1, Owner: owner}, s.beginErr
}
func (s *stubTransitions) BeginConfig(ctx context.Context, scope, owner, op string, now time.Time, _ AccountDecisionGuard) (AccountTransitionTicket, error) {
	return s.Begin(ctx, scope, owner, op, now)
}
func (s *stubTransitions) Finish(context.Context, AccountTransitionTicket, AccountPublication) error {
	return s.finishErr
}
func (s *stubTransitions) Abort(context.Context, AccountTransitionTicket) error { return s.finishErr }

func TestHeldTransitions669(t *testing.T) {
	ctx := context.Background()
	stub := &stubTransitions{}
	h := TrackHeldTransitions(stub)
	stub.held = h

	tk, err := h.Begin(ctx, "ns:a", "o1", "op", time.Now())
	if err != nil || !stub.sawHeld || !h.Holds("o1") {
		t.Fatalf("the owner is not held before and after Begin (saw %v, holds %v, err %v)", stub.sawHeld, h.Holds("o1"), err)
	}
	stub.finishErr = errors.New("finish failed")
	if err := h.Finish(ctx, tk, AccountPublication{}); err == nil || !h.Holds("o1") {
		t.Fatal("a failed Finish released the ticket")
	}
	stub.finishErr = nil
	if err := h.Finish(ctx, tk, AccountPublication{}); err != nil || h.Holds("o1") {
		t.Fatal("a successful Finish kept the ticket")
	}

	tk2, _ := h.BeginConfig(ctx, "ns:a", "o2", "op", time.Now(), nil)
	if err := h.Abort(ctx, tk2); err != nil || h.Holds("o2") {
		t.Fatal("a successful Abort kept the ticket")
	}

	stub.beginErr = ErrAccountTransition
	if _, err := h.Begin(ctx, "ns:a", "o3", "op", time.Now()); err == nil || h.Holds("o3") {
		t.Fatal("a failed Begin left the owner in the set")
	}
	if _, ok := any(h).(AccountTransitionRecovery); !ok {
		t.Fatal("the tracked repository does not offer recovery")
	}
	if _, _, err := h.ListBusy(ctx); !errors.Is(err, ErrTransitionRecoveryUnsupported) {
		t.Errorf("recovery over a repository without it: err %v", err)
	}
}

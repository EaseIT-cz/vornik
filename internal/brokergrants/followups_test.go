package brokergrants_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
)

// Review 20261003-f819 follow-ups to the broker write-actions design's
// "Tier 2 as built".

// f819 item 3: a destination must be address-shaped, so a recipient list
// holding a non-string element is refused at KeyOf (no offer, no coverage)
// rather than compared in order.
func TestKeyOf_RefusesANonStringDestination(t *testing.T) {
	dest := map[string]bool{"to": true, "cc": true}
	for _, args := range []string{
		`{"to":"a@x.com","cc":["b@x.com",7]}`,
		`{"to":"a@x.com","cc":[{"addr":"b@x.com"}]}`,
		`{"to":42}`,
	} {
		if _, err := brokergrants.KeyOf([]string{"to", "cc"}, dest, []byte(args)); err == nil {
			t.Errorf("%s: a non-address destination was accepted", args)
		}
	}
	if _, err := brokergrants.KeyOf([]string{"to", "cc"}, dest, []byte(`{"to":"a@x.com"}`)); err != nil {
		t.Errorf("an absent cc must stay valid: %v", err)
	}
}

// f819 item 2: an action whose key cannot be computed is not covered, and the
// miss is visible (its own reason), not silent.
func TestCover_UncomputableKeyIsACountedMiss(t *testing.T) {
	e := newEnv(t)
	e.seed(`{"to":"a@x.com"}`, 7, 20)
	a := e.action("ns1--mail", `{"to":"a@x.com","cc":["b@x.com",7]}`)
	if c, err := e.svc.Cover(e.ctx, a); c || err != nil {
		t.Fatalf("covered %v, err %v", c, err)
	}
	if got := e.metrics.Miss(brokergrants.MissUnkeyed); got != 1 {
		t.Fatalf("miss{%s} = %v, want 1", brokergrants.MissUnkeyed, got)
	}
	if o := e.svc.Offer(a); o != nil {
		t.Fatal("an uncomputable key was offered a grant")
	}
}

// failingSuspend makes Suspend fail, as a store error would.
type failingSuspend struct {
	persistence.BrokerGrantRepository
}

func (failingSuspend) Suspend(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("store down")
}

// f819 item 1: a Suspend that did not happen is not counted as one, nor is
// the miss labelled suspended; the grant still does not cover (fail safe).
func TestCover_FailedSuspendIsNotCountedAsSuspended(t *testing.T) {
	e := newEnv(t)
	e.seed(`{"to":"a@x.com"}`, 7, 20)
	e.reach["ns1--mail"] = "reach-2"
	e.svc = e.rebuild(failingSuspend{e.grants})
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"a@x.com"}`)); c {
		t.Fatal("covered after the reach changed")
	}
	if e.metrics.Suspended() != 0 || e.metrics.Miss(brokergrants.MissSuspended) != 0 {
		t.Fatalf("a failed suspend was counted: suspended %v, miss %v", e.metrics.Suspended(), e.metrics.Miss(brokergrants.MissSuspended))
	}
	if e.metrics.Miss(brokergrants.MissSuspendFailed) != 1 {
		t.Fatalf("miss{%s} = %v", brokergrants.MissSuspendFailed, e.metrics.Miss(brokergrants.MissSuspendFailed))
	}
}

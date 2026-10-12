//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: finding D. Every decision took SELECT ... FOR UPDATE on
// the one shared daemon row, so staging and grant decisions in different
// namespaces serialised on it. Decisions now lock FOR SHARE; a transition
// still locks FOR UPDATE and so still waits for a held decision.
func TestAccountTransition_DecisionLocksDoNotSerialiseAcrossNamespaces(t *testing.T) {
	a, b, _ := accountFaultPools(t)
	ctx := context.Background()
	for _, ns := range []string{"alpha", "beta"} {
		if _, err := NewAccountTransitionRepository(a).ReadDecisionEpoch(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	held, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback() }()
	if _, err = NewAccountTransitionRepository(held).ReadDecisionEpoch(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}

	other, err := b.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.ExecContext(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err = NewAccountTransitionRepository(other).ReadDecisionEpoch(ctx, "beta"); err != nil {
		t.Fatalf("a decision in another namespace waited on the daemon row: %v", err)
	}
	_ = other.Rollback()

	begin, err := b.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = begin.Rollback() }()
	if _, err = begin.ExecContext(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err = NewAccountTransitionRepository(begin).Begin(ctx, persistence.AccountDaemonScope, "owner", "test", time.Now()); err == nil {
		t.Fatal("a daemon Begin did not wait for a held decision lock")
	}
}

package repotest

// GitHub #669 item 3 (design 22, §22.2.16b): the controlled-recovery CAS on
// both drivers. A dead holder is a row a Begin left busy with no process
// behind it, the state the 2026-10-28 crash left. The race is pinned in both
// deterministic orders, and a holder whose row a recovery cleared cannot
// Finish (AGENTS.md §3: no stochastic loop).

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AccountTransitionRecoveryRepo is both halves the suite needs.
type AccountTransitionRecoveryRepo interface {
	persistence.AccountTransitionRepository
	persistence.AccountTransitionRecovery
}

const (
	recoveryScope = "ns:hermes"
	recoveryDead  = "dead0000000000000000000000000000"
	recoveryLive  = "live1111111111111111111111111111"
)

var recoveryNow = time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)

// RunAccountTransitionRecoverySuite runs the recovery contract. fresh
// returns an empty repository per subtest.
func RunAccountTransitionRecoverySuite(t *testing.T, fresh func(t *testing.T) AccountTransitionRecoveryRepo) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, AccountTransitionRecoveryRepo)
	}{
		{"Recover_clears_one_busy_row_and_advances_the_epoch", recoveryClearsOne},
		{"A_wrong_epoch_owner_or_idle_row_changes_nothing", recoveryRefusesMismatch},
		{"Order_a_Begin_first_fails_busy_then_Recover_succeeds", recoveryOrderA},
		{"Order_b_Recover_first_then_Begin_then_a_second_Recover_is_refused", recoveryOrderB},
		{"A_cleared_holder_cannot_Finish", recoveryClearedHolder},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, fresh(t)) })
	}
}

func recoveryBusy(t *testing.T, r AccountTransitionRecoveryRepo) persistence.AccountTransitionTicket {
	t.Helper()
	tk, err := r.Begin(context.Background(), recoveryScope, recoveryDead, "connection-config", recoveryNow)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func recoveryClearsOne(t *testing.T, r AccountTransitionRecoveryRepo) {
	ctx := context.Background()
	tk := recoveryBusy(t, r)
	rows, examined, err := r.ListBusy(ctx)
	if err != nil || examined != 1 || len(rows) != 1 {
		t.Fatalf("ListBusy = %+v, examined %d, err %v", rows, examined, err)
	}
	if rows[0].Owner != recoveryDead || rows[0].Operation != "connection-config" || rows[0].Epoch != tk.Epoch {
		t.Fatalf("busy row = %+v", rows[0])
	}
	before, err := r.Recover(ctx, recoveryScope, tk.Epoch, recoveryDead)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Busy || before.Owner != recoveryDead || before.Epoch != tk.Epoch || before.Operation != "connection-config" || before.StartedAt == "" {
		t.Errorf("before-values = %+v", before)
	}
	if rows, examined, _ := r.ListBusy(ctx); len(rows) != 0 || examined != 1 {
		t.Errorf("after recovery: busy %+v, examined %d", rows, examined)
	}
	next, err := r.Begin(ctx, recoveryScope, recoveryLive, "oauth-publication", recoveryNow)
	if err != nil || next.Epoch != tk.Epoch+2 {
		t.Errorf("Begin after recovery: epoch %d (want %d), err %v", next.Epoch, tk.Epoch+2, err)
	}
}

func recoveryRefusesMismatch(t *testing.T, r AccountTransitionRecoveryRepo) {
	ctx := context.Background()
	tk := recoveryBusy(t, r)
	for _, c := range []struct {
		name  string
		epoch int64
		owner string
	}{{"wrong epoch", tk.Epoch + 1, recoveryDead}, {"wrong owner", tk.Epoch, recoveryLive}} {
		if _, err := r.Recover(ctx, recoveryScope, c.epoch, c.owner); !errors.Is(err, persistence.ErrAccountTransition) {
			t.Errorf("%s: err %v, want ErrAccountTransition", c.name, err)
		}
	}
	if rows, _, _ := r.ListBusy(ctx); len(rows) != 1 || rows[0].Epoch != tk.Epoch {
		t.Errorf("a refused recovery changed the row: %+v", rows)
	}
	if _, err := r.Recover(ctx, "ns:idle", 0, recoveryDead); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Errorf("an absent row: err %v", err)
	}
}

func recoveryOrderA(t *testing.T, r AccountTransitionRecoveryRepo) {
	ctx := context.Background()
	tk := recoveryBusy(t, r)
	if _, err := r.Begin(ctx, recoveryScope, recoveryLive, "oauth-publication", recoveryNow); err == nil {
		t.Fatal("a Begin on a busy row succeeded")
	}
	if _, err := r.Recover(ctx, recoveryScope, tk.Epoch, recoveryDead); err != nil {
		t.Fatalf("Recover after the refused Begin: %v", err)
	}
}

func recoveryOrderB(t *testing.T, r AccountTransitionRecoveryRepo) {
	ctx := context.Background()
	tk := recoveryBusy(t, r)
	if _, err := r.Recover(ctx, recoveryScope, tk.Epoch, recoveryDead); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, recoveryScope, recoveryLive, "oauth-publication", recoveryNow); err != nil {
		t.Fatalf("Begin after Recover: %v", err)
	}
	if _, err := r.Recover(ctx, recoveryScope, tk.Epoch, recoveryDead); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Errorf("a second Recover of the cleared ticket: err %v", err)
	}
}

func recoveryClearedHolder(t *testing.T, r AccountTransitionRecoveryRepo) {
	ctx := context.Background()
	tk := recoveryBusy(t, r)
	if _, err := r.Recover(ctx, recoveryScope, tk.Epoch, recoveryDead); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, tk, persistence.AccountPublication{}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Errorf("the cleared holder's Finish: err %v, want ErrAccountTransition", err)
	}
}

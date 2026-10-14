package controlplane

import (
	"context"
	"errors"
	"testing"
)

// GitHub #302 (2026-10-13): the approver-device card for a DRIFT row is filed
// when the row parks and withdrawn when any door resolves it (config-apply
// journal design §13.3). The engine signals both through two optional,
// best-effort hooks.

func TestOnDrift_FiresWhenARowParks(t *testing.T) {
	env, pid := readSetCase(t, "verifying")
	var fired []string
	env.e.OnDrift = func(_ context.Context, journalID string) { fired = append(fired, journalID) }
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, pid)
	if len(fired) != 1 || fired[0] != j.ID {
		t.Fatalf("OnDrift saw %v, want [%s]", fired, j.ID)
	}
}

func TestOnResolved_FiresAfterAResultOutsideTheLocks(t *testing.T) {
	env, _, jid := driftCase(t)
	var fired []string
	lockFree := false
	env.e.OnResolved = func(_ context.Context, journalID string) {
		fired = append(fired, journalID)
		// The hook runs after the resolve released its locks, so a hook
		// that reaches the engine cannot deadlock it.
		if configWriteMu.TryLock() {
			configWriteMu.Unlock()
			lockFree = true
		}
	}
	if _, err := env.e.ResolveJournal(context.Background(), jid, ResolveKeep, "vadim"); err != nil {
		t.Fatal(err)
	}
	// The idempotent repeat is a result too, so it fires again.
	if _, err := env.e.ResolveJournal(context.Background(), jid, ResolveKeep, "vadim"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 || fired[0] != jid || !lockFree {
		t.Fatalf("OnResolved saw %v (lock free %v), want twice %s outside the lock", fired, lockFree, jid)
	}
	// A refusal (the other mode) is not a result: no hook.
	if _, err := env.e.ResolveJournal(context.Background(), jid, ResolveRestore, "vadim"); err == nil {
		t.Fatal("the other mode on a resolved row must be refused")
	}
	if len(fired) != 2 {
		t.Errorf("OnResolved fired on a refusal: %v", fired)
	}
}

// #334 audit 2026-10-14: the OnDrift contract said hooks run "outside the
// engine's locks", but parkInDrift calls OnDrift while the config write lock
// and the engine lock are held. The contract now says so: an OnDrift hook
// must not block and must not re-enter the engine (the wired hook in
// internal/service/journal_cards.go files its card on a goroutine). This
// pins the as-built so the comment and the code cannot drift apart again.
func TestOnDrift_RunsUnderTheEngineLocks(t *testing.T) {
	env, _ := readSetCase(t, "verifying")
	held := false
	env.e.OnDrift = func(context.Context, string) {
		writeFree := configWriteMu.TryLock()
		if writeFree {
			configWriteMu.Unlock()
		}
		engineFree := env.e.mu.TryLock()
		if engineFree {
			env.e.mu.Unlock()
		}
		held = !writeFree && !engineFree
	}
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	if !held {
		t.Fatal("OnDrift ran with a lock free; update the OnDrift contract in apply.go and the design §13.3")
	}
}

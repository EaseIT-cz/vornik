package controlplane

import (
	"context"
	"errors"
	"testing"
)

// GitHub #579 (agent-administered design part 22, §22.2.16a). Found at:
// production (2026-10-28): a reload-rejected apply the engine reverted and
// verified still left the namespace's account transition busy, because the
// service could not tell a verified rollback from a possible publish.
// Regression test at: integration, the journaled engine against a real config
// tree (newJournalEnv). Lower because: production needed a daemon-wide reload
// failure; the engine path that returns it is the same.

func TestJournal_579_ARevertedReloadSaysSo(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	calls := 0
	reloadErr := errors.New("bad yaml")
	env.e.Reload = func() error {
		calls++
		if calls == 1 {
			return reloadErr
		}
		return nil
	}
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrReverted) {
		t.Fatalf("a reverted, restored apply must wrap ErrReverted: %v", err)
	}
	if !errors.Is(err, reloadErr) {
		t.Fatalf("the cause must still be wrapped: %v", err)
	}
}

func TestJournal_579_ADriftedRevertDoesNotSayReverted(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.e.Reload = func() error { return errors.New("bad yaml") }
	env.crashAt("reverting", false, func() { env.write(t, "projects/a.yaml", "operator: edited\n") })
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrJournalDrift) || errors.Is(err, ErrReverted) {
		t.Fatalf("a drifted revert restored nothing for certain and must not claim ErrReverted: %v", err)
	}
}

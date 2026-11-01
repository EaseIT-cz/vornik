package controlplane

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #584 (operations-api-contract-design.md §6.6): the control plane's
// apply lock is the process's one config-write lock. While any other writer
// holds it (a rotation, a setup commit, a feature enable), an apply is
// refused as busy, as a second apply already is; it succeeds after.
func TestConfigLock584_ControlPlaneRefusesWhileHeld(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	unlock := configlock.Lock()
	err := env.e.Apply(context.Background(), id, "vadim", false)
	unlock()
	if !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("apply while another writer holds the config lock = %v, want ErrApplyInProgress", err)
	}
	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply after the lock was released: %v", err)
	}
}

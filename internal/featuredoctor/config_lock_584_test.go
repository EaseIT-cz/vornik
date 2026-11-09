package featuredoctor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #584 (operations-api-contract-design.md §6.6): LockConfigFile, the
// lock ApplyEnable and integrations.Save take, is the process's one
// config-write lock, so they serialise with every other writer, not only
// with each other on the same path.
func TestConfigLock584_LockConfigFileIsTheOneLock(t *testing.T) {
	for _, path := range []string{"/etc/vornik/vornik.yaml", "/etc/vornik/configs/projects/p.yaml"} {
		unlock := lockForTest(path)
		if _, ok := configlock.TryLock(); ok {
			t.Fatalf("configlock was free while LockConfigFile(%q) was held", path)
		}
		unlock()
	}
	release, ok := configlock.TryLock()
	if !ok {
		t.Fatal("configlock stayed held after LockConfigFile released")
	}
	release()
}

// lockForTest takes LockConfigFile with no writer check registered, which
// always allows (GitHub #681).
func lockForTest(path string) func() {
	unlock, err := LockConfigFile(context.Background(), path)
	if err != nil {
		panic(err)
	}
	return unlock
}

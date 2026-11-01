package configlock

import "testing"

func TestConfigLock584_TryLockRefusesWhileHeld(t *testing.T) {
	unlock := Lock()
	if _, ok := TryLock(); ok {
		t.Fatal("TryLock took a held lock")
	}
	unlock()
	release, ok := TryLock()
	if !ok {
		t.Fatal("TryLock refused a free lock")
	}
	release()
}

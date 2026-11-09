package projectarchive

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #584 (operations-api-contract-design.md §6.6): archiving patches a
// project's config file, and took no lock (found checking the writer
// census, not by its patterns). It now waits for the process's one
// config-write lock like every other writer.
func TestConfigLock584_ProjectArchiveWaits(t *testing.T) {
	svc, path := newServiceFixture(t, "p", &passthroughPatcher{})
	svc.Patcher = func(in []byte, _ []PatchOp) ([]byte, error) {
		return append(append([]byte{}, in...), []byte("archived: true\n")...), nil
	}
	before, _ := os.ReadFile(path)

	unlock := configlock.Lock()
	done := make(chan error, 1)
	go func() { done <- svc.applyPatches(context.Background(), "p", nil) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the archive patch finished while another held the config lock (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	if during, _ := os.ReadFile(path); !bytes.Equal(before, during) {
		unlock()
		t.Fatal("the project file changed while another writer held the lock")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); bytes.Equal(before, after) {
		t.Fatal("the archive patch wrote nothing")
	}
}

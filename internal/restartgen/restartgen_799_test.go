package restartgen

// GitHub #799 (operations-API design §6.13): a persisted restart generation,
// bumped on every restart-pending mark and read at start, so the control
// plane can tell a change saved during its restart from a restart that did
// not apply.
//
// Found at: review (vornik-cloud control-plane design 5.5, apply_restart),
// observed at unit (nothing counts marks or survives a restart).

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newFile(t *testing.T) (*File, string) {
	t.Helper()
	dir := t.TempDir()
	return New(filepath.Join(dir, "config.yaml")), filepath.Join(dir, "restart-generation")
}

func TestRestartGen799_MissingIsZero(t *testing.T) {
	f, _ := newFile(t)
	if n, ok := f.Read(); !ok || n != 0 {
		t.Fatalf("missing file: %d %v, want 0 true", n, ok)
	}
}

func TestRestartGen799_BumpsCountAndSurvive(t *testing.T) {
	f, path := newFile(t)
	for i := 0; i < 2; i++ {
		if err := f.Bump(); err != nil {
			t.Fatal(err)
		}
	}
	if n, ok := f.Read(); !ok || n != 2 {
		t.Fatalf("after two bumps: %d %v", n, ok)
	}
	// A new process reading the same directory sees the same count.
	if n, ok := New(filepath.Join(filepath.Dir(path), "config.yaml")).Read(); !ok || n != 2 {
		t.Fatalf("a new reader: %d %v", n, ok)
	}
}

func TestRestartGen799_ConcurrentBumpsAreNotLost(t *testing.T) {
	f, _ := newFile(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.Bump() }()
	}
	wg.Wait()
	if n, ok := f.Read(); !ok || n != 50 {
		t.Fatalf("50 concurrent bumps gave %d %v", n, ok)
	}
}

func TestRestartGen799_ACorruptFileIsUnknownNotZero(t *testing.T) {
	f, path := newFile(t)
	if err := os.WriteFile(path, []byte("not a number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Read(); ok {
		t.Fatal("a corrupt file read as known")
	}
}

func TestRestartGen799_AFailedWriteIsAnError(t *testing.T) {
	f := New(filepath.Join(t.TempDir(), "missing-dir", "config.yaml"))
	if err := f.Bump(); err == nil {
		t.Fatal("a bump into a missing directory reported success")
	}
}

package featuredoctor

// TDD for the shared path-keyed config-file write lock
// (https://docs.vornik.io). Same file
// serializes; different files run concurrently; equivalent path strings share
// one lock.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

func TestLockConfigFile_SameFileSerializes(t *testing.T) {
	unlock := lockForTest("/tmp/vornik-a.yaml")
	acquired := make(chan struct{})
	go func() {
		u2 := lockForTest("/tmp/vornik-a.yaml")
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		t.Fatal("second lock on the same path acquired while the first was held")
	case <-time.After(60 * time.Millisecond):
		// good — blocked as expected
	}
	unlock()
	select {
	case <-acquired:
		// good — acquired once released
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired after the first released")
	}
}

// GitHub #584 reversed this test's former property ("a different path must
// not block"): writers holding locks on different paths, or different locks,
// interleaved on one config file. Every writer now holds the one lock, so a
// different path waits too.
func TestLockConfigFile_DifferentFilesSerialiseToo(t *testing.T) {
	u1 := lockForTest("/tmp/vornik-a.yaml")
	acquired := make(chan struct{})
	go func() {
		u2 := lockForTest("/tmp/vornik-b.yaml")
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		u1()
		t.Fatal("a lock on a different path did not wait for the one config-write lock")
	case <-time.After(60 * time.Millisecond):
	}
	u1()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the second lock was never acquired after the first released")
	}
}

func TestLockConfigFile_CleanEquivalence(t *testing.T) {
	unlock := lockForTest("/x/c.yaml")
	acquired := make(chan struct{})
	go func() {
		u2 := lockForTest("/x/./c.yaml") // filepath.Clean-equivalent → same lock
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		t.Fatal("equivalent path string did not share the lock")
	case <-time.After(60 * time.Millisecond):
	}
	unlock()
	<-acquired
}

// TestLockConfigFile_PreventsLostUpdate is the regression: two RMW transactions
// on the SAME file, each adding a distinct key under the lock, must both survive.
func TestLockConfigFile_PreventsLostUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("base: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rmw := func(key string) {
		unlock := lockForTest(path)
		defer unlock()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		out, _, err := config.SetYAMLKey(content, key, "on")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Error(err)
		}
	}
	var wg sync.WaitGroup
	for _, k := range []string{"integrations.slack", "featuredoctor.gate"} {
		wg.Add(1)
		go func(key string) { defer wg.Done(); rmw(key) }(k)
	}
	wg.Wait()
	final, _ := os.ReadFile(path)
	for _, want := range []string{"slack:", "gate:", "base:"} {
		if !strings.Contains(string(final), want) {
			t.Errorf("lost update: %q missing from final config:\n%s", want, final)
		}
	}
}

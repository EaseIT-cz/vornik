package secretstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadOrCreateKey_CreatesOnce0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets", "store.key")
	k1, err := LoadOrCreateKey(p)
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %d bytes, %v", len(k1), err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(p)
	if err != nil || string(k1) != string(k2) {
		t.Fatal("second call did not load the same key")
	}
}

// Review Focus 3: concurrent first use yields ONE key, so no row is sealed
// under a key that is then overwritten.
func TestLoadOrCreateKey_RaceHasOneWinner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.key")
	var wg sync.WaitGroup
	keys := make([][]byte, 16)
	for i := range keys {
		wg.Add(1)
		go func(i int) { defer wg.Done(); keys[i], _ = LoadOrCreateKey(p) }(i)
	}
	wg.Wait()
	for i := range keys {
		if string(keys[i]) != string(keys[0]) {
			t.Fatalf("goroutine %d got a different key", i)
		}
	}
}

// Review Focus 2: a missing or damaged key is ErrKeyUnavailable, never a fresh key.
func TestLoadKey_MissingOrWrongSize(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(filepath.Join(dir, "absent")); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("absent: %v", err)
	}
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(short, []byte("abc"), 0o600)
	if _, err := LoadKey(short); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("short: %v", err)
	}
}

// Plan review f329 finding 4: a .tmp left by a crash does not block first use.
func TestLoadOrCreateKey_StaleTmpDoesNotBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.key")
	if err := os.WriteFile(p+".tmp", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute)
	_ = os.Chtimes(p+".tmp", old, old)
	k, err := LoadOrCreateKey(p)
	if err != nil || len(k) != 32 {
		t.Fatalf("stale tmp blocked creation: %d bytes, %v", len(k), err)
	}
}

// Review 20261002-34a5 suggestion 1: a young .tmp belongs to a creator that
// has not linked yet (preempted, slow disk). It is left alone: the caller
// waits for the link and, when none comes, reports the key unavailable
// rather than removing the other creator's file and minting a second key.
func TestLoadOrCreateKey_YoungTmpIsLeftToItsCreator(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.key")
	if err := os.WriteFile(p+".tmp", []byte("in-flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	young := time.Now().Add(-30 * time.Second)
	_ = os.Chtimes(p+".tmp", young, young)
	if _, err := LoadOrCreateKey(p); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable while another creator holds the tmp", err)
	}
	if b, err := os.ReadFile(p + ".tmp"); err != nil || string(b) != "in-flight" {
		t.Fatalf("the in-flight tmp was disturbed: %q, %v", b, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("a second key was minted beside the in-flight one")
	}
}

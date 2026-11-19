package benchlearning

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), change 3
// and its revision): learning is frozen between the warm-up and the measured
// pass. A tick runs under a shared lock the freeze takes exclusively, and the
// freeze survives a restart.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGate809_RunsUntilFrozenAndStaysFrozenAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	g, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	if !g.Do(func() { ran++ }) || ran != 1 {
		t.Fatal("a tick did not run before the freeze")
	}
	at, err := g.Freeze()
	if err != nil || at.IsZero() {
		t.Fatalf("freeze: %v %v", at, err)
	}
	if g.Do(func() { ran++ }) || ran != 1 {
		t.Fatal("a tick ran after the freeze")
	}
	again, err := g.Freeze()
	if err != nil || !again.Equal(at) {
		t.Fatalf("a second freeze moved the time: %v vs %v (%v)", again, at, err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, frozen := reopened.FrozenAt(); !frozen || !got.Equal(at) {
		t.Fatalf("after a restart: frozen %v at %v, want %v", frozen, got, at)
	}
	if reopened.Do(func() { ran++ }) {
		t.Fatal("a tick ran after a restart of a frozen gate")
	}
}

func TestGate809_TheFreezeWaitsForATickInFlight(t *testing.T) {
	g, _ := Open(t.TempDir())
	inTick, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.Do(func() { close(inTick); <-release })
	}()
	<-inTick
	done := make(chan struct{})
	go func() { _, _ = g.Freeze(); close(done) }()
	select {
	case <-done:
		t.Fatal("the freeze returned while a tick was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	<-done
}

func TestGate809_ANilGateAlwaysRuns(t *testing.T) {
	var g *Gate
	ran := false
	if !g.Do(func() { ran = true }) || !ran {
		t.Fatal("a daemon with no bench gate must tick as before")
	}
}

// A crash between writing and renaming leaves only the temporary file,
// which is not a freeze.
func TestGate809_ALeftoverTemporaryFileIsNotAFreeze(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FrozenFile+".tmp"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := g.FrozenAt(); frozen {
		t.Fatal("a leftover temporary file read as a freeze")
	}
	if _, err := g.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FrozenFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("the temporary file is still there after the freeze")
	}
}

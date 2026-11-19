package github

import (
	"os"
	"testing"
	"time"
)

// The package's other tests share the process-wide gates and make many
// writes on one installation: they run without the spacing.
func TestMain(m *testing.M) {
	writeSpacing = 0
	os.Exit(m.Run())
}

func TestPace244_HoldSpacingAndBound(t *testing.T) {
	writeSpacing = time.Second
	t.Cleanup(func() { writeSpacing = 0 })
	g := &installGate{}
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	if w, err := g.admit(true, now); w != 0 || err != nil {
		t.Fatalf("first write: %v %v", w, err)
	}
	if w, err := g.admit(true, now); w != writeSpacing || err != nil {
		t.Fatalf("second write: wait %v %v, want %v", w, err, writeSpacing)
	}
	if w, err := g.admit(false, now); w != 0 || err != nil {
		t.Fatal("a read is not spaced")
	}
	// Writes queued past the bound fail.
	for i := 0; i < 29; i++ {
		_, _ = g.admit(true, now)
	}
	if _, err := g.admit(true, now); err == nil {
		t.Fatal("a write queued past 30 s was admitted")
	}
	// A hold refuses reads and writes until it lifts.
	g2 := &installGate{}
	g2.hold(now, 2*time.Second)
	if _, err := g2.admit(false, now.Add(time.Second)); err == nil {
		t.Fatal("a read went out during a hold")
	}
	if _, err := g2.admit(true, now.Add(3*time.Second)); err != nil {
		t.Fatalf("a call after the hold: %v", err)
	}
	first := gateFor("https://api.github.com", 7)
	if again := gateFor("https://api.github.com", 7); again != first || first == gateFor("https://api.github.com", 8) {
		t.Fatal("gates are not shared per installation")
	}
}

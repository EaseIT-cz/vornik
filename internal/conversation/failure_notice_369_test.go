package conversation

import (
	"testing"
	"time"
)

// GitHub #369 item 4: one notice per key per hour, pruned on write.
func TestNoticeWindow369_OncePerKeyPerHour(t *testing.T) {
	var w NoticeWindow
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if !w.Allow("thread-a", t0) {
		t.Fatal("the first notice was refused")
	}
	if w.Allow("thread-a", t0.Add(59*time.Minute)) {
		t.Fatal("a second notice within the hour was allowed")
	}
	if !w.Allow("thread-b", t0.Add(time.Minute)) {
		t.Fatal("another key was refused: the window is per key")
	}
	if !w.Allow("thread-a", t0.Add(61*time.Minute)) {
		t.Fatal("a notice after the hour was refused")
	}
	w.Allow("thread-c", t0.Add(3*time.Hour))
	if len(w.last) != 1 {
		t.Fatalf("entries older than the hour were kept: %v", w.last)
	}
}

func TestNoticeWindow369_AFailedSendDoesNotSpendTheWindow(t *testing.T) {
	var w NoticeWindow
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	w.Allow("k", t0)
	w.Forget("k")
	if !w.Allow("k", t0.Add(time.Minute)) {
		t.Fatal("a forgotten key was still suppressed")
	}
}

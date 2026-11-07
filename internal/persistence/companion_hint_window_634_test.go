package persistence

// GitHub #634 slice 2 (companion hardening design §13 H5, §13.10 S2): the
// back-off rule, one implementation for both drivers.

import (
	"strings"
	"testing"
	"time"
)

func events(pattern string) []bool {
	out := make([]bool, 0, len(pattern))
	for _, c := range pattern {
		out = append(out, c == '1')
	}
	return out
}

var hintNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func apply(w HintWindow, ev []bool, now time.Time) HintWindow {
	got, _ := ApplyHintEvents(w, ev, now)
	return got
}

// 10 firings with 1 followed back off for 14 days; with 2 they do not.
func TestApplyHintEvents_BackOffAtFullWindow_634(t *testing.T) {
	got := apply(HintWindow{}, events("1000000000"), hintNow)
	if got.BackoffUntil == nil || !got.BackoffUntil.Equal(hintNow.Add(HintBackoff)) {
		t.Fatalf("1 of 10 followed must back off until now+14d: %+v", got)
	}
	got = apply(HintWindow{}, events("1100000000"), hintNow)
	if got.BackoffUntil != nil {
		t.Fatalf("2 of 10 followed is 20%%, not under it: %+v", got)
	}
	if got.Window != "1100000000" || got.FiredTotal != 10 || got.FollowedTotal != 2 {
		t.Fatalf("window and lifetime counts: %+v", got)
	}
}

// 9 firings with none followed leave the hint active: only a full window is
// judged.
func TestApplyHintEvents_PartialWindowStaysActive_634(t *testing.T) {
	got := apply(HintWindow{}, events("000000000"), hintNow)
	if got.BackoffUntil != nil || got.Window != "000000000" {
		t.Fatalf("9 firings are not a full window: %+v", got)
	}
}

// The window keeps the last 10, in order.
func TestApplyHintEvents_TrimsToLastTen_634(t *testing.T) {
	w := apply(HintWindow{}, events("1111111111"), hintNow)
	w = apply(w, events("000"), hintNow)
	if w.Window != "1111111000" {
		t.Fatalf("oldest dropped first: %q", w.Window)
	}
	if w.FiredTotal != 13 || w.FollowedTotal != 10 {
		t.Fatalf("lifetime counts keep everything: %+v", w)
	}
}

// Events while a back-off runs are dropped; after it ends the window starts
// empty and is not judged again before 10 new firings.
func TestApplyHintEvents_BackOffDropsThenResets_634(t *testing.T) {
	w := apply(HintWindow{}, events("0000000000"), hintNow)
	if w.BackoffUntil == nil {
		t.Fatal("0 of 10 backs off")
	}
	during := apply(w, events("111"), hintNow.Add(24*time.Hour))
	if during.Window != w.Window || during.FiredTotal != w.FiredTotal {
		t.Fatalf("events during a back-off are dropped: %+v", during)
	}
	after := apply(w, events("000000000"), hintNow.Add(HintBackoff))
	if after.BackoffUntil != nil || after.Window != "000000000" {
		t.Fatalf("after the back-off: empty window, then 9 new firings, active: %+v", after)
	}
}

// A batch that fills the window and backs off drops the rest of the batch.
func TestApplyHintEvents_BatchCrossingThreshold_634(t *testing.T) {
	got := apply(HintWindow{}, events("000000000011111"), hintNow)
	if got.BackoffUntil == nil || got.Window != "0000000000" || got.FiredTotal != 10 {
		t.Fatalf("the back-off stops the batch: %+v", got)
	}
}

// The input row is not modified, and Window never exceeds 10 characters.
func TestApplyHintEvents_Pure_634(t *testing.T) {
	in := HintWindow{Window: "1"}
	_ = apply(in, events(strings.Repeat("1", 30)), hintNow)
	if in.Window != "1" {
		t.Fatal("input mutated")
	}
	if got := apply(in, events(strings.Repeat("1", 30)), hintNow); len(got.Window) != HintWindowSize {
		t.Fatalf("window length %d", len(got.Window))
	}
}

// The window is judged after every append (review 6550 F1): a full window
// with exactly 2 follows backs off at the append that pushes one out.
func TestApplyHintEvents_JudgedAfterEveryAppend_634(t *testing.T) {
	w := apply(HintWindow{}, events("1100000000"), hintNow)
	if w.BackoffUntil != nil {
		t.Fatal("2 of 10 is active")
	}
	w = apply(w, events("0"), hintNow)
	if w.BackoffUntil == nil || w.Window != "1000000000" {
		t.Fatalf("pushing a follow out leaves 1 of 10: backed off: %+v", w)
	}
}

// At backoff_until exactly, the back-off has ended (review 6550 minor).
func TestApplyHintEvents_BackoffBoundary_634(t *testing.T) {
	w := apply(HintWindow{}, events("0000000000"), hintNow)
	got, applied := ApplyHintEvents(w, events("1"), *w.BackoffUntil)
	if got.BackoffUntil != nil || got.Window != "1" || applied != 1 {
		t.Fatalf("at the end instant the window restarts: %+v applied=%d", got, applied)
	}
}

// The applied count excludes events dropped by a back-off (review 6550 F6).
func TestApplyHintEvents_AppliedCount_634(t *testing.T) {
	_, applied := ApplyHintEvents(HintWindow{}, events("000000000011111"), hintNow)
	if applied != 10 {
		t.Fatalf("applied = %d, want 10", applied)
	}
}

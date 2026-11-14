package api

import "testing"

// A web chat turn outlives its request (web-chat-design.md §2.3): it holds
// the in-flight count the deploy gate waits on, and a draining daemon refuses
// it, releasing the count it took (§8.5a's order: count first, bit second).
func TestTrackLongWork_CountsAndReleases_WebChat(t *testing.T) {
	s := &Server{}
	done, ok := s.TrackLongWork()
	if !ok || s.inflight.chatProxy.Load() != 1 {
		t.Fatalf("ok=%v count=%d, want true and 1", ok, s.inflight.chatProxy.Load())
	}
	done()
	done() // idempotent
	if n := s.inflight.chatProxy.Load(); n != 0 {
		t.Fatalf("count after done = %d, want 0", n)
	}
	s.draining.Store(true)
	if _, ok := s.TrackLongWork(); ok {
		t.Fatal("a draining daemon must refuse new long work")
	}
	if n := s.inflight.chatProxy.Load(); n != 0 {
		t.Fatalf("a refusal must release its count, got %d", n)
	}
}

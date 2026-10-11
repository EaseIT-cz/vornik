package service

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
)

// GitHub #97 (2026-10-07 deploy cut an in-flight chat stream): the chat-proxy
// wrapper refuses new requests from the moment beginDrain sets the drain bit,
// before the grace sleep. The bit beginDrain sets must be the one the API
// server reads.
func TestBeginDrain_SetsTheBitTheChatWrapperReads(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")
	srv := api.NewServer()
	c := &Container{Logger: zerolog.Nop(), apiServer: srv}
	if srv.IsDraining() {
		t.Fatal("a fresh server is draining")
	}
	c.beginDrain()
	if !srv.IsDraining() {
		t.Fatal("beginDrain did not set the API server's drain bit")
	}
}

// GitHub #290 (2026-10-08): the deploy gate now sets the drain bit through
// the operator drain verb before SIGTERM. beginDrain's grace (time for an
// upstream router to notice /readyz) has then already run for as long as the
// bit has been set: it sleeps only the remainder, and the SIGTERM drain is
// permanent (no expiry or undrain can clear it during shutdown).
func TestBeginDrain_SleepsOnlyTheGraceNotYetElapsed(t *testing.T) {
	for name, tc := range map[string]struct {
		drainedAgo time.Duration // < 0: no operator drain
		want       []time.Duration
	}{
		"no operator drain: the full grace":       {-1, []time.Duration{5 * time.Second}},
		"drained 10s ago: no sleep":               {10 * time.Second, nil},
		"drained exactly the grace ago: no sleep": {5 * time.Second, nil},
		"drained 2s ago: the remaining 3s":        {2 * time.Second, []time.Duration{3 * time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "5")
			base := time.Now()
			now := base
			srv := api.NewServer(api.WithDrainClock(func() time.Time { return now }))
			if tc.drainedAgo >= 0 {
				if _, err := srv.BeginOperatorDrain(time.Minute); err != nil {
					t.Fatal(err)
				}
				now = base.Add(tc.drainedAgo)
			}
			var slept []time.Duration
			orig := drainSleep
			drainSleep = func(d time.Duration) { slept = append(slept, d) }
			t.Cleanup(func() { drainSleep = orig })

			c := &Container{Logger: zerolog.Nop(), apiServer: srv}
			c.beginDrain()

			if len(slept) != len(tc.want) || (len(slept) == 1 && slept[0] != tc.want[0]) {
				t.Fatalf("slept %v, want %v", slept, tc.want)
			}
			if !srv.IsDraining() {
				t.Fatal("not draining after beginDrain")
			}
			if err := srv.EndOperatorDrain(); err == nil {
				t.Fatal("an undrain succeeded during shutdown; the SIGTERM drain must be permanent")
			}
			if !srv.IsDraining() {
				t.Fatal("the bit was cleared during shutdown")
			}
		})
	}
}

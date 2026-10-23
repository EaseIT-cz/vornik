package agenthealth

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// GitHub #363 (permanent-failure design §8.8.2 D1): the time a retry after a
// MODEL_UNHEALTHY reject is held to comes from the breaker, never from the
// message, and is never more than one cooldown away.
func TestNextAdmission_363(t *testing.T) {
	cooldown := agentCfg().OpenCooldown
	infra := errors.New("PROVIDER_ERROR: upstream provider returned an error")
	setup := func(t *testing.T) (*Registry, *fakeClock) {
		t.Helper()
		clk := &fakeClock{t: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
		r := NewRegistry(Config{Health: agentCfg(), Enabled: true, Now: clk.now})
		return r, clk
	}
	trip := func(r *Registry, model string) {
		for i := 0; i < 3; i++ {
			r.Record(model, false, infra)
		}
	}
	reject := func(t *testing.T, r *Registry, model string) error {
		t.Helper()
		permitted, _, err := r.Gate(model)
		if permitted || err == nil {
			t.Fatalf("setup: %s must be rejected", model)
		}
		return err
	}

	t.Run("open: the half-open time, read from the breaker", func(t *testing.T) {
		r, clk := setup(t)
		opened := clk.t
		trip(r, "m1")
		clk.advance(10 * time.Second)
		err := fmt.Errorf("step work: %w", reject(t, r, "m1"))
		at, basis, ok := r.NextAdmission(err)
		if !ok || !at.Equal(opened.Add(cooldown)) || basis != AdmissionHalfOpenAt {
			t.Fatalf("got %v %q %v, want %v %q", at, basis, ok, opened.Add(cooldown), AdmissionHalfOpenAt)
		}
	})

	t.Run("re-opened since the error: the breaker's time, not the error's OpenSince", func(t *testing.T) {
		r, clk := setup(t)
		trip(r, "m1")
		err := reject(t, r, "m1")
		clk.advance(cooldown)
		if permitted, probe, _ := r.Gate("m1"); !permitted || !probe {
			t.Fatal("setup: the cooldown passed, this caller must be the probe")
		}
		reopened := clk.t
		r.Record("m1", true, infra) // the probe fails: OPEN again, from now
		at, _, ok := r.NextAdmission(err)
		if !ok || !at.Equal(reopened.Add(cooldown)) {
			t.Fatalf("got %v %v, want %v", at, ok, reopened.Add(cooldown))
		}
	})

	t.Run("closed now: no hold", func(t *testing.T) {
		r, clk := setup(t)
		trip(r, "m1")
		err := reject(t, r, "m1")
		clk.advance(cooldown)
		_, probe, _ := r.Gate("m1")
		r.Record("m1", probe, nil) // the probe succeeds
		if _, _, ok := r.NextAdmission(err); ok {
			t.Fatal("a circuit that is closed now admits at once: no hold")
		}
	})

	t.Run("half-open now (a probe in flight): one cooldown from now", func(t *testing.T) {
		r, clk := setup(t)
		trip(r, "m1")
		clk.advance(cooldown)
		if _, probe, _ := r.Gate("m1"); !probe {
			t.Fatal("setup: the first caller after the cooldown is the probe")
		}
		err := reject(t, r, "m1") // half_open reject
		at, basis, ok := r.NextAdmission(err)
		if !ok || !at.Equal(clk.t.Add(cooldown)) || basis != AdmissionProbeInFlight {
			t.Fatalf("got %v %q %v, want %v %q", at, basis, ok, clk.t.Add(cooldown), AdmissionProbeInFlight)
		}
	})

	backoffCases := map[string]error{
		"marker only, the chain flattened to text": errors.New(`Adaptive routing failed (last step: route, reason: MODEL_UNHEALTHY: model "m1" on route "agent" circuit open (open since 2026-10-17T08:00:00Z))`),
		"a chat-router reject":                     &chat.ModelUnhealthyError{Route: "ollama_cloud", Model: "m1", State: "open", OpenSince: time.Now()},
		"a model the registry has no breaker for":  &chat.ModelUnhealthyError{Route: "agent", Model: "never-seen", State: "open"},
	}
	for name, err := range backoffCases {
		t.Run("backoff: "+name, func(t *testing.T) {
			r, clk := setup(t)
			at, basis, ok := r.NextAdmission(err)
			if !ok || !at.Equal(clk.t.Add(cooldown)) || basis != AdmissionBackoff {
				t.Fatalf("got %v %q %v, want %v %q", at, basis, ok, clk.t.Add(cooldown), AdmissionBackoff)
			}
			if _, loaded := r.reg.Load("never-seen"); loaded {
				t.Fatal("NextAdmission must not create a breaker")
			}
		})
	}

	t.Run("clamped to one cooldown", func(t *testing.T) {
		r, clk := setup(t)
		trip(r, "m1")
		err := reject(t, r, "m1")
		clk.advance(-time.Hour) // the clock stepped back: openedAt is now in the future
		at, _, ok := r.NextAdmission(err)
		if !ok || at.After(clk.t.Add(cooldown)) {
			t.Fatalf("a hold must never exceed one cooldown: got %v, now %v", at, clk.t)
		}
	})

	noHold := map[string]error{
		"nil":                 nil,
		"not a circuit error": infra,
	}
	for name, err := range noHold {
		t.Run("no hold: "+name, func(t *testing.T) {
			r, _ := setup(t)
			if _, _, ok := r.NextAdmission(err); ok {
				t.Fatal("only a MODEL_UNHEALTHY failure is held")
			}
		})
	}
	t.Run("no hold: disabled or nil registry", func(t *testing.T) {
		mu := &chat.ModelUnhealthyError{Route: "agent", Model: "m1", State: "open"}
		if _, _, ok := NewRegistry(Config{Health: agentCfg(), Enabled: false}).NextAdmission(mu); ok {
			t.Fatal("a disabled registry holds nothing")
		}
		var nilReg *Registry
		if _, _, ok := nilReg.NextAdmission(mu); ok {
			t.Fatal("a nil registry holds nothing")
		}
	})
}

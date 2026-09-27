package service

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// One declared speed factor serves the lease AND every step timeout
// (dynamic-tool-budget design §6.2.1b, 2026-09-26). Incident: the
// slow-hardware bench arm, where speed_aware_timeouts scaled only the lease.
func TestSpeedFactor(t *testing.T) {
	mk := func(sat config.SpeedAwareTimeoutsConfig) *Container {
		c := &Container{Config: &config.Config{}, Logger: zerolog.Nop()}
		c.Config.Scheduler.SpeedAwareTimeouts = sat
		return c
	}
	base := config.SpeedAwareTimeoutsConfig{Enabled: true, ReferenceTokensPerSec: 80, ObservedTokensPerSec: 20, MinFactor: 0.5, MaxFactor: 8}

	if f, clamped := mk(base).speedFactor(); f != 4 || clamped {
		t.Fatalf("80/20 = %v (clamped %v), want 4", f, clamped)
	}
	off := base
	off.Enabled = false
	if f, _ := mk(off).speedFactor(); f != 1 {
		t.Fatalf("disabled must be 1, got %v", f)
	}
	noObs := base
	noObs.ObservedTokensPerSec = 0
	if f, _ := mk(noObs).speedFactor(); f != 1 {
		t.Fatalf("no observed rate must be 1, got %v", f)
	}
	noRef := base
	noRef.ReferenceTokensPerSec = 0
	if f, _ := mk(noRef).speedFactor(); f != 1 {
		t.Fatalf("an unusable reference must be 1, got %v", f)
	}
	slow := base
	slow.ObservedTokensPerSec = 4
	if f, clamped := mk(slow).speedFactor(); f != 8 || !clamped {
		t.Fatalf("80/4 = 20 must clamp to 8, got %v (clamped %v)", f, clamped)
	}
	fast := base
	fast.ObservedTokensPerSec = 400
	if f, clamped := mk(fast).speedFactor(); f != 0.5 || clamped {
		t.Fatalf("a host faster than the reference floors at min_factor without a clamp warning, got %v (clamped %v)", f, clamped)
	}
	// The lease reads the same factor.
	if got := mk(base).scaleLeaseForSpeed(300); got != 1200 {
		t.Fatalf("lease 300s × 4 = %d, want 1200", got)
	}
}

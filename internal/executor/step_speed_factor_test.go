package executor

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// Step timeouts take the declared speed factor (dynamic-tool-budget design
// §6.2.1b, 2026-09-26). Incident: the slow-hardware bench arm, where the
// tier-scaled step budgets were the binding limit and speed_aware_timeouts
// scaled only the lease, which the scheduler already renews.

func TestApplySpeedFactor(t *testing.T) {
	ephemeral := &registry.SwarmRole{Name: "coder", RuntimePolicy: "ephemeral"}
	warm := &registry.SwarmRole{Name: "lead", RuntimePolicy: "warm"}
	floor := 5 * time.Minute
	cases := []struct {
		name           string
		budget, native time.Duration
		factor         float64
		role           *registry.SwarmRole
		want           time.Duration
	}{
		{"factor 2 doubles the tier-scaled budget", 15 * time.Minute, 30 * time.Minute, 2, ephemeral, 30 * time.Minute},
		{"factor 1 is today", 15 * time.Minute, 30 * time.Minute, 1, ephemeral, 15 * time.Minute},
		{"factor 0.5 halves an ephemeral budget", 40 * time.Minute, 40 * time.Minute, 0.5, ephemeral, 20 * time.Minute},
		{"factor 0.5 stops at the floor", 8 * time.Minute, 8 * time.Minute, 0.5, ephemeral, 5 * time.Minute},
		{"the floor never raises above native", 3 * time.Minute, 3 * time.Minute, 0.5, ephemeral, 3 * time.Minute},
		{"a warm role is stretched", 30 * time.Minute, 30 * time.Minute, 2, warm, 60 * time.Minute},
		{"a warm role is never shrunk", 30 * time.Minute, 30 * time.Minute, 0.5, warm, 30 * time.Minute},
		{"no timeout stays no timeout", 0, 0, 4, ephemeral, 0},
		{"a zero factor is today", 15 * time.Minute, 30 * time.Minute, 0, ephemeral, 15 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := applySpeedFactor(c.budget, c.native, c.factor, c.role, floor); got != c.want {
				t.Fatalf("applySpeedFactor = %s, want %s", got, c.want)
			}
		})
	}
}

// The counterfactual cap is applied LAST, after the speed factor, and only
// lowers (design §6.2.1b ordering).
func TestResolveStepTimeout_OrderingAndCap(t *testing.T) {
	ephemeral := &registry.SwarmRole{Name: "coder", RuntimePolicy: "ephemeral"}
	cfg := budgetCfg()
	// 30m native, complex tier (1.0), speed 2 → 60m; a 20m cap wins.
	if got := resolveStepTimeout(30*time.Minute, ephemeral, "complex", false, cfg, 2, 20*time.Minute); got != 20*time.Minute {
		t.Fatalf("cap after speed = %s, want 20m", got)
	}
	// A cap above the scaled budget changes nothing.
	if got := resolveStepTimeout(30*time.Minute, ephemeral, "complex", false, cfg, 2, 90*time.Minute); got != 60*time.Minute {
		t.Fatalf("a loose cap = %s, want 60m", got)
	}
	// Tier then speed: standard (0.5) × speed 4 on 30m → 60m.
	if got := resolveStepTimeout(30*time.Minute, ephemeral, "standard", false, cfg, 4, 0); got != 60*time.Minute {
		t.Fatalf("tier × speed = %s, want 60m", got)
	}
	// Disabled everything (speed 1, tool budget off) is today.
	off := cfg
	off.Enabled = false
	if got := resolveStepTimeout(30*time.Minute, ephemeral, "trivial", false, off, 1, 0); got != 30*time.Minute {
		t.Fatalf("all off = %s, want the native 30m", got)
	}
}

// The per-call LLM timeout is derived from the step budget, so it follows the
// speed factor with no wiring of its own (design §6.2.1b): on a trivial task a
// 30m step is 7.5m and caps a call at 225s; at speed 4 it is 30m and the
// call's own ceiling decides.
func TestPerCallTimeout_FollowsTheSpeedScaledStep(t *testing.T) {
	ephemeral := &registry.SwarmRole{Name: "reviewer", RuntimePolicy: "ephemeral"}
	cfg := budgetCfg()
	unscaled := resolveStepTimeout(30*time.Minute, ephemeral, "trivial", false, cfg, 1, 0)
	if got := perCallTimeoutForStep(unscaled, 900*time.Second); got != 225*time.Second {
		t.Fatalf("trivial 30m step, speed 1: per-call = %s, want 225s", got)
	}
	scaled := resolveStepTimeout(30*time.Minute, ephemeral, "trivial", false, cfg, 4, 0)
	if got := perCallTimeoutForStep(scaled, 900*time.Second); got != 900*time.Second {
		t.Fatalf("trivial 30m step, speed 4: per-call = %s, want the 900s ceiling", got)
	}
}

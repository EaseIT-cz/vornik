package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GitHub #703 (found in production 2026-11-09): fallback_rungs was ERR with
// four rungs none of which was a live problem — one whose model was removed
// from the role's config on 2026-11-09, three on a model that now works on
// dozens of roles. The check read only the 30-day ledger, so it cannot tell
// "configured now and failing" from "fixed or removed weeks ago", and it kept
// alarming for up to 30 days after the operator fixed the config. An ERR that
// a fix does not clear teaches the operator to ignore the row.
//
// The verdict now joins each rung against the resolved config (a rung whose
// (role, model) fallback binding no longer exists is history) and against the
// model's own behaviour in the window (a model that has succeeded since the
// rung's last failure makes the rung historical, not live), and publishes the
// denominator: rungs examined, how many are still configured.

func configuredBindings(pairs ...[2]string) func(role, model string) bool {
	set := map[string]bool{}
	for _, p := range pairs {
		set[p[0]+"\x00"+p[1]] = true
	}
	return func(role, model string) bool { return set[role+"\x00"+model] }
}

// alwaysConfigured is the pre-#703 behaviour for the older tests: every rung
// still configured, no model known to work, so the verdict is exactly what the
// ledger alone used to produce.
var alwaysConfigured = func(_, _ string) bool { return true }

// A rung whose model is no longer configured for its role is history, not an
// ERROR. Fails pre-fix: the same input produced status ERROR.
func TestFallbackRungs703_RungNoLongerConfiguredIsNotAnError(t *testing.T) {
	last := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	items, status, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "review_model_fallback", role: "reviewer", model: "zai.glm-5.3",
		attempts: 3, lastClass: "llm_call_failed", lastFailed: last,
	}}, configuredBindings(), nil) // nothing configured

	if status != "OK" {
		t.Fatalf("a rung no longer configured for its role must not alarm, got %q: %q", status, msg)
	}
	if len(items) != 0 {
		t.Fatalf("stale rungs are history, not findings: %v", items)
	}
	if !strings.Contains(msg, "no longer configured") {
		t.Errorf("the message must say the dead rung was checked against the config and found gone: %q", msg)
	}
}

// A rung still configured, but whose model has SUCCEEDED since the rung's
// last failure, is history until the rung is next reached: WARN with the
// last-failure date, not ERR. Fails pre-fix: status was ERROR.
func TestFallbackRungs703_ModelWorkingSinceLastFailureIsWarnNotError(t *testing.T) {
	last := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	items, status, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "route_model_fallback", role: "lead", model: "glm-5.2",
		attempts: 4, calledAndFailed: 4, lastClass: "llm_call_failed", lastFailed: last,
	}}, configuredBindings([2]string{"lead", "glm-5.2"}),
		map[string]time.Time{"glm-5.2": last.Add(24 * time.Hour)})

	if status != "WARNING" {
		t.Fatalf("a model that has worked since the rung's last failure is not a live fault, got %q: %q", status, msg)
	}
	if len(items) != 1 || !strings.Contains(items[0], "2026-10-10") {
		t.Fatalf("the WARN must carry the last-failure date: %v", items)
	}
	if !strings.Contains(msg, "since") {
		t.Errorf("the message must say the model has succeeded since the last failure: %q", msg)
	}
}

// The original defect stays an ERROR: a rung still configured whose model has
// shown no success in the window is live.
func TestFallbackRungs703_LiveDeadRungStillErrors(t *testing.T) {
	last := time.Now().UTC()
	items, status, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "discover_model_fallback", role: "scout", model: "gemma4:26b",
		attempts: 4, lastClass: "model_unhealthy", lastFailed: last,
	}}, configuredBindings([2]string{"scout", "gemma4:26b"}), nil)

	if status != "ERROR" {
		t.Fatalf("a still-configured dead rung on a model with no success must stay an ERROR, got %q: %q", status, msg)
	}
	if len(items) != 1 || !strings.Contains(items[0], "discover_model_fallback") {
		t.Fatalf("want the live rung as a finding: %v", items)
	}
}

// The denominator (tenets §4): how many dead rungs were examined, and how many
// are still configured — not only how many were found.
func TestFallbackRungs703_DenominatorIsPublished(t *testing.T) {
	last := time.Now().UTC()
	_, status, msg := evaluateFallbackRungs([]deadFallbackRung{
		{stepID: "a_model_fallback", role: "lead", model: "glm-5.2", attempts: 2, lastFailed: last},
		{stepID: "b_model_fallback", role: "reviewer", model: "gone-model", attempts: 2, lastFailed: last},
	}, configuredBindings([2]string{"lead", "glm-5.2"}), nil)

	if status != "ERROR" {
		t.Fatalf("one live rung keeps the check at ERROR, got %q", status)
	}
	for _, want := range []string{"2 dead", "1 still configured", "1 no longer configured"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks the denominator clause %q: %q", want, msg)
		}
	}
}

// A success OLDER than the rung's last failure proves nothing about the rung:
// the model may have broken again. Only an ok AFTER lastFailed downgrades.
func TestFallbackRungs703_OldSuccessDoesNotDowngrade(t *testing.T) {
	last := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	_, status, _ := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "a_model_fallback", role: "lead", model: "glm-5.2",
		attempts: 2, lastClass: "llm_call_failed", lastFailed: last,
	}}, configuredBindings([2]string{"lead", "glm-5.2"}),
		map[string]time.Time{"glm-5.2": last.Add(-time.Hour)}) // ok BEFORE the last failure
	if status != "ERROR" {
		t.Fatalf("a success predating the last failure must not downgrade a live fault, got %q", status)
	}
}

// queryWorkingModels pins the second half of the join on SQLite (the driver
// the 2026-09-04 extension found this check silent on): model → newest ok in
// the window, models without an ok absent, empty models ignored.
func TestQueryWorkingModels703_MostRecentOkPerModel(t *testing.T) {
	db := newModelHealth665DB(t)
	now := time.Now().UTC()
	// The repositories write recorded_at as RFC3339Nano text; the fixture must
	// do the same, or the driver's own time.Time formatting lands in a shape
	// timestampValue (and the lexicographic MAX) does not read.
	add := func(model, outcome string, at time.Time) {
		if _, err := db.Exec(`INSERT INTO execution_step_outcomes (model, outcome, recorded_at) VALUES (?,?,?)`,
			model, outcome, at.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	add("glm-5.2", "ok", now.Add(-48*time.Hour))
	add("glm-5.2", "ok", now.Add(-2*time.Hour)) // newest wins
	add("glm-5.2", "failed", now.Add(-time.Minute))
	add("gemma4:26b", "failed", now.Add(-time.Hour))
	add("", "ok", now.Add(-time.Hour))
	add("outside-window", "ok", now.Add(-40*24*time.Hour))

	got, err := queryWorkingModels(context.Background(), db, now.Add(-30*24*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	want := now.Add(-2 * time.Hour)
	if !got["glm-5.2"].Equal(want) {
		t.Fatalf("glm-5.2 newest ok = %v, want %v", got["glm-5.2"], want)
	}
}

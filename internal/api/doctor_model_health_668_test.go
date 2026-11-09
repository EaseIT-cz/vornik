package api

import (
	"math/rand"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// GitHub #668 (model-health attribution design, amendment 2026-11-07 later).
// Two doctor runs an hour apart recommended different fallbacks for glm-5.2:
// collectModelFallbacks kept one fallback per model, first role seen wins,
// over Registry.ListSwarms' map walk. On the live tree glm-5.2 is pinned by
// 26 roles with three different fallbacks, and is another role's fallback
// too, which made it read "no fallback configured".

// oneFallback is the fallbacks argument for a model pinned by one role.
func oneFallback(model, fallback string) map[string]map[string]int {
	return map[string]map[string]int{model: {fallback: 1}}
}

func swarms668() []*registry.Swarm {
	return []*registry.Swarm{
		{ID: "a", Roles: []registry.SwarmRole{
			{Name: "lead", Model: "X", ModelFallback: "Y"},
			{Name: "writer", Model: "W", ModelFallback: "X"}, // X is also a fallback
		}},
		{ID: "b", Roles: []registry.SwarmRole{
			{Name: "lead", Model: "X", ModelFallback: "Y"},
			{Name: "coder", Model: "X", ModelFallback: "Z"},
			{Name: "self", Model: "S", ModelFallback: "S"}, // names its own primary
		}},
	}
}

func recommendationFor668(t *testing.T, sw []*registry.Swarm, model string) string {
	t.Helper()
	referenced, fallbacks := collectModelFallbacks(sw)
	if !referenced[model] {
		t.Fatalf("%s not referenced", model)
	}
	stats := []modelHealthStat{{model: model, samples: 20, failures: 20}}
	findings := evalModelHealth(stats, fallbacks, defaultDoctorThresholds())
	if len(findings) != 1 {
		t.Fatalf("want one finding for %s, got %v", model, findings)
	}
	return findings[0].message
}

// The same advice whatever order the swarms and roles are walked in.
func TestCollectModelFallbacks668_OrderDoesNotChangeTheAdvice(t *testing.T) {
	want := `recommend switching to the configured modelFallback "Y" (2 roles), "Z" (1 role)`
	rng := rand.New(rand.NewSource(668))
	for i := 0; i < 50; i++ {
		sw := swarms668()
		rng.Shuffle(len(sw), func(a, b int) { sw[a], sw[b] = sw[b], sw[a] })
		for _, s := range sw {
			rng.Shuffle(len(s.Roles), func(a, b int) { s.Roles[a], s.Roles[b] = s.Roles[b], s.Roles[a] })
		}
		if got := recommendationFor668(t, sw, "X"); !strings.Contains(got, want) {
			t.Fatalf("walk %d: %q lacks %q", i, got, want)
		}
	}
}

// A fallback that names the primary is another call to the same model: the
// executor's hop runs and retries it. Named as such, never recommended.
func TestCollectModelFallbacks668_ASelfFallbackIsNamed(t *testing.T) {
	got := recommendationFor668(t, swarms668(), "S")
	if strings.Contains(got, `"S"`) || !strings.HasPrefix(got, "S: ") ||
		!strings.Contains(got, "no fallback configured") ||
		!strings.Contains(got, "1 role(s) name the model itself as modelFallback, which retries the same model") {
		t.Fatalf("a self-fallback must be named, not recommended: %q", got)
	}
}

// Roles with a fallback, with none, and with a self-fallback: all reported.
func TestCollectModelFallbacks668_AllThreeKinds(t *testing.T) {
	sw := []*registry.Swarm{{ID: "a", Roles: []registry.SwarmRole{
		{Name: "r1", Model: "X", ModelFallback: "Y"},
		{Name: "r2", Model: "X"},
		{Name: "r3", Model: "X", ModelFallback: "X"},
	}}}
	// The three kinds partition the roles pinning X (review 44b2).
	if _, fb := collectModelFallbacks(sw); fb["X"]["Y"]+fb["X"][""]+fb["X"]["X"] != 3 {
		t.Fatalf("counts %v do not add up to the 3 roles pinning X", fb["X"])
	}
	got := recommendationFor668(t, sw, "X")
	for _, want := range []string{`"Y" (1 role)`, "1 role(s) pinning it have no other model as modelFallback",
		"1 role(s) name the model itself as modelFallback"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
}

// A model that appears only as a fallback pins no role and gets no
// recommendation. The fallback of a role with an empty model is the same.
func TestCollectModelFallbacks668_AFallbackOnlyModel(t *testing.T) {
	sw := append(swarms668(), &registry.Swarm{ID: "c", Roles: []registry.SwarmRole{{Name: "r", ModelFallback: "E"}}})
	for _, m := range []string{"Y", "E"} {
		if got := recommendationFor668(t, sw, m); !strings.Contains(got, "no fallback configured") {
			t.Fatalf("fallback-only model %s: %q", m, got)
		}
	}
	referenced, fallbacks := collectModelFallbacks(sw)
	if referenced[""] || len(fallbacks["E"]) != 0 {
		t.Fatalf("an empty model pins nothing: referenced %v, entries %v", referenced[""], fallbacks["E"])
	}
}

// Every role entry counts: "lead" in swarms a and b is two roles.
func TestCollectModelFallbacks668_SameRoleNameInTwoSwarms(t *testing.T) {
	_, fallbacks := collectModelFallbacks(swarms668())
	if fallbacks["X"]["Y"] != 2 || fallbacks["X"]["Z"] != 1 {
		t.Fatalf("X's counts = %v, want Y:2 Z:1", fallbacks["X"])
	}
}

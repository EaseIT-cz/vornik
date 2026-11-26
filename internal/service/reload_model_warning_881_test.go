package service

// GitHub #881, registry design §12.2 C4: a reload cannot refuse a file that is
// already written, so it warns, once per model, about each model it newly
// introduces that no router route serves. It makes no call.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestUnroutedNewModels_881(t *testing.T) {
	swarms := []*registry.Swarm{
		{ID: "a", Roles: []registry.SwarmRole{{Name: "r1", Model: "routed"}, {Name: "r2", Model: "lost", ModelFallback: "lost"}}},
		{ID: "b", Roles: []registry.SwarmRole{{Name: "r3", Model: "lost"}, {Name: "r4", Model: "known-before"}}},
	}
	seen := map[string]bool{"known-before": true}
	resolves := func(m string) (string, bool) { return "route", m == "routed" }
	got := unroutedNewModels(swarms, seen, resolves)
	if strings.Join(got, ",") != "lost" {
		t.Fatalf("got %v, want [lost]: one warning per model, not per role", got)
	}
	if !seen["lost"] || !seen["routed"] {
		t.Error("seen must remember every model, so the next reload does not warn again")
	}
	if again := unroutedNewModels(swarms, seen, resolves); len(again) != 0 {
		t.Errorf("a second reload warned again: %v", again)
	}
	if none := unroutedNewModels(swarms, map[string]bool{}, nil); len(none) != 0 {
		t.Errorf("no router: %v, want no warnings", none)
	}
}

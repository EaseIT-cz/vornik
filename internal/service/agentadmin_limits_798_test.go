package service

// GitHub #798 and #805 (part 06 §6.9). Found at: production (the #793 case
// study). Test at: integration (the real service, State loaded from the
// config tree and the registry): the caps reach the verbs, and
// describe_installation states them. Without the wiring every value is
// refused, since a State with no caps admits nothing.

import (
	"context"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

func TestAgentAdmin798_CapsReachTheVerbsAndDescribe(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if caps.Limits.MaxConcurrentTasks != 2 || caps.Limits.MaxRoleTokens != 32768 || caps.Limits.MaxRoleToolIterations != 120 || caps.Limits.DefaultToolIterations != 30 {
		t.Fatalf("limits = %+v", caps.Limits)
	}
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	got := f.do(agentadmin.VerbUpdateProject, json.RawMessage(`{"project":"finance","max_concurrent_tasks":2}`))
	if got.Effect == agentadmin.EffectRefused {
		t.Fatalf("a value within the default cap was refused: %+v", got)
	}
	got = f.do(agentadmin.VerbUpdateProject, json.RawMessage(`{"project":"finance","max_concurrent_tasks":3}`))
	if got.Effect != agentadmin.EffectRefused {
		t.Fatalf("a value above the cap was not refused: %+v", got)
	}
}

package service

// GitHub #550 (found at: production, 2026-10-25). Design:
// https://docs.vornik.io §2.1 C1. The
// policy of an executor task key comes from the role of the execution's
// CURRENT step in the execution's OWN workflow: the architect's workflow is
// a non-default published one (#551 is the same lookup done against the
// default workflow).

import (
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestResolveMemorySearchRolePolicy(t *testing.T) {
	pol := &registry.MemorySearchPolicy{RepoScope: "github.com/x/y", Authority: "provenance"}
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "consultant", MemorySearch: pol},
		{Name: "reviewer"},
	}}
	workflows := map[string]*registry.Workflow{
		"vornik-architecture": {ID: "vornik-architecture", Steps: map[string]registry.WorkflowStep{"answer": {Role: "consultant"}}},
		"default":             {ID: "default", Steps: map[string]registry.WorkflowStep{"answer": {Role: "reviewer"}}},
	}
	getWF := func(id string) *registry.Workflow { return workflows[id] }
	getSwarm := func(projectID string) (*registry.Swarm, error) {
		if projectID != "p" {
			return nil, errors.New("unknown project")
		}
		return swarm, nil
	}
	step := func(s string) *string { return &s }

	cases := []struct {
		name    string
		exec    *persistence.Execution
		want    *registry.MemorySearchPolicy
		wantErr string
	}{
		{"the execution's own workflow decides the role", &persistence.Execution{ProjectID: "p", WorkflowID: "vornik-architecture", CurrentStepID: step("answer")}, pol, ""},
		{"a role without a policy", &persistence.Execution{ProjectID: "p", WorkflowID: "default", CurrentStepID: step("answer")}, nil, ""},
		{"no execution", nil, nil, "no execution"},
		{"no current step", &persistence.Execution{ProjectID: "p", WorkflowID: "vornik-architecture"}, nil, "current step"},
		{"unknown workflow", &persistence.Execution{ProjectID: "p", WorkflowID: "gone", CurrentStepID: step("answer")}, nil, "workflow"},
		{"step not in the workflow", &persistence.Execution{ProjectID: "p", WorkflowID: "vornik-architecture", CurrentStepID: step("nope")}, nil, "step"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMemorySearchRolePolicy(tc.exec, getWF, getSwarm)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	t.Run("a role missing from the swarm fails closed", func(t *testing.T) {
		exec := &persistence.Execution{ProjectID: "p", WorkflowID: "vornik-architecture", CurrentStepID: step("answer")}
		empty := func(string) (*registry.Swarm, error) { return &registry.Swarm{}, nil }
		if _, err := resolveMemorySearchRolePolicy(exec, getWF, empty); err == nil {
			t.Fatal("want an error: the step's role is not in the swarm")
		}
	})
	t.Run("a swarm lookup error fails closed", func(t *testing.T) {
		exec := &persistence.Execution{ProjectID: "p", WorkflowID: "vornik-architecture", CurrentStepID: step("answer")}
		broken := func(string) (*registry.Swarm, error) { return nil, errors.New("not loaded") }
		if _, err := resolveMemorySearchRolePolicy(exec, getWF, broken); err == nil {
			t.Fatal("want an error")
		}
	})
}

// A warm-pool key names its role directly (security review 2026-10-25,
// finding 1): the policy is that role's in the key's project swarm, and a
// role the swarm does not hold, or no role at all, is an error (refused).
func TestResolveWarmRoleMemorySearchPolicy(t *testing.T) {
	pol := &registry.MemorySearchPolicy{RepoScope: "github.com/x/y", Authority: "provenance"}
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{{Name: "consultant", MemorySearch: pol}, {Name: "reviewer"}}}
	getSwarm := func(projectID string) (*registry.Swarm, error) {
		if projectID != "p" {
			return nil, errors.New("unknown project")
		}
		return swarm, nil
	}
	if got, err := resolveWarmRoleMemorySearchPolicy("p", "consultant", getSwarm); err != nil || got != pol {
		t.Fatalf("consultant: %+v, %v", got, err)
	}
	if got, err := resolveWarmRoleMemorySearchPolicy("p", "reviewer", getSwarm); err != nil || got != nil {
		t.Fatalf("reviewer: %+v, %v; want no policy", got, err)
	}
	for _, c := range []struct{ project, role string }{{"p", "ghost"}, {"p", ""}, {"other", "consultant"}} {
		if _, err := resolveWarmRoleMemorySearchPolicy(c.project, c.role, getSwarm); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

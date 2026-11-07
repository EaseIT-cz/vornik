package service

// GitHub #618 C4: doctor's memory_sensitivity_cap counts each capped role's
// project under the role's whole filter, the same one its searches get.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/registry"
)

func TestSensitivityCapRoles_CountsUnderTheRolesFilter_618(t *testing.T) {
	const scope = "github.com/example/repo"
	swarms := map[string]*registry.Swarm{
		"arch": {Roles: []registry.SwarmRole{
			{Name: "consultant", MemorySearch: &registry.MemorySearchPolicy{RepoScope: scope, StrictScope: true, Authority: "provenance",
				SourcePrefixes: []string{"docs/public/"}, MaxSensitivity: "public"}},
			{Name: "uncapped", MemorySearch: &registry.MemorySearchPolicy{RepoScope: scope, StrictScope: true}},
			{Name: "plain"},
		}},
		"broken": {Roles: []registry.SwarmRole{
			{Name: "r", MemorySearch: &registry.MemorySearchPolicy{RepoScope: scope, Authority: "any", MaxSensitivity: "internal"}},
		}},
	}
	projects := []*registry.Project{{ID: "broken"}, {ID: "arch"}, {ID: "missing"}}
	swarmOf := func(id string) (*registry.Swarm, error) {
		if sw, ok := swarms[id]; ok {
			return sw, nil
		}
		return nil, errors.New("no swarm")
	}
	var got []memory.ScopeFilter
	count := func(_ context.Context, projectID string, sf memory.ScopeFilter) (memory.SensitivityCapCounts, error) {
		got = append(got, sf)
		if projectID == "broken" {
			return memory.SensitivityCapCounts{}, errors.New("db down")
		}
		return memory.SensitivityCapCounts{DerivedPublic: 7, Unlabelled: 9, Admitted: 7}, nil
	}
	rows := sensitivityCapRoles(context.Background(), projects, swarmOf, []string{"docs/public/", "docs/architect/"}, count)
	if len(rows) != 2 || rows[0].ProjectID != "arch" || rows[0].Role != "consultant" || rows[1].ProjectID != "broken" {
		t.Fatalf("rows %+v, want the two capped roles, sorted", rows)
	}
	if rows[0].Admitted != 7 || rows[0].DerivedPublic != 7 || rows[0].Unlabelled != 9 || rows[0].Err != nil {
		t.Errorf("arch row %+v", rows[0])
	}
	if rows[1].Err == nil {
		t.Errorf("a count error must reach the row: %+v", rows[1])
	}
	wantArch := memory.ScopeFilter{RepoScope: scope, Strict: true, Authority: memory.AuthorityProvenance,
		SourcePrefixes: []string{"docs/public/"}, MaxSensitivity: "public", PublicPrefixes: []string{"docs/public/", "docs/architect/"}}
	found := false
	for _, sf := range got {
		if reflect.DeepEqual(sf, wantArch) {
			found = true
		}
		if sf.RepoScope == scope && sf.MaxSensitivity == "internal" && sf.Authority != memory.AuthorityAny {
			t.Errorf("authority any must map to the memory package's empty value, got %q", sf.Authority)
		}
	}
	if !found {
		t.Errorf("the consultant's count must use its whole filter %+v; got %+v", wantArch, got)
	}
}

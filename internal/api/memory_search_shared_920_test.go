package api

// GitHub #920 (agent-administered design part 24 §24.2 A2): a job step's
// memory_search, authorized on its own project, reads the namespace's shared
// memory, so one job finds what a sibling job stored. A key of another
// project is still refused before any search.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// projStub records the project each search ran against.
type projStub struct{ got []string }

func (p *projStub) Search(_ context.Context, project, _ string, _ int) ([]MemorySearchResult, error) {
	p.got = append(p.got, project)
	return nil, nil
}

func (p *projStub) SearchRouting(_ context.Context, project, _ string, _ MemorySearchRequest) ([]MemorySearchResult, *RoutingVerdictWire, error) {
	p.got = append(p.got, project)
	return nil, nil, nil
}

func sharedSearch(t *testing.T, urlProject string, key *persistence.APIKey) (*projStub, int) {
	t.Helper()
	stub := &projStub{}
	srv := NewServer(WithMemorySearcher(stub),
		WithMemoryProjectResolver(func(p string) string { return agentns.MemoryProject(p, p == "hermes--prague-venues") }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+urlProject+"/memory/search?q=venues", nil)
	// As the auth middleware leaves it: auth on, the key scoped to its project.
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{key.ProjectID})
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: key}})
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, r.WithContext(ContextWithAPIKeyIDForTesting(ctx, key.ID)))
	return stub, rec.Code
}

func TestMemorySearch_AgentJobReadsSharedMemory_920(t *testing.T) {
	job := &persistence.APIKey{ID: "k", Name: persistence.TaskKeyNamePrefix + "task_1",
		ProjectID: "hermes--prague-venues", CreatedBy: executorKeyCreator}
	stub, code := sharedSearch(t, "hermes--prague-venues", job)
	if code != http.StatusOK || len(stub.got) != 1 || stub.got[0] != "hermes--home" {
		t.Fatalf("code %d, searched %v; want 200 and the namespace's memory project", code, stub.got)
	}

	op := &persistence.APIKey{ID: "k2", Name: persistence.TaskKeyNamePrefix + "task_2", ProjectID: "dev-project", CreatedBy: executorKeyCreator}
	stub, code = sharedSearch(t, "dev-project", op)
	if code != http.StatusOK || len(stub.got) != 1 || stub.got[0] != "dev-project" {
		t.Fatalf("an operator project searched %v, want its own", stub.got)
	}

	// Authorization is on the URL's project, before resolution: a job key
	// cannot reach another project's memory by naming it.
	stub, code = sharedSearch(t, "other-project", job)
	if code != http.StatusForbidden || len(stub.got) != 0 {
		t.Fatalf("a foreign project: code %d, searched %v; want 403 and no search", code, stub.got)
	}
}

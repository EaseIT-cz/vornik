package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// #618 C1 (firewall design, "Amendment 2026-11-06 (#618)"): the role's
// maxSensitivity reaches the search for the role's own key, the query string
// cannot set or lift it, and the retrieval_policy block names it. Before the
// change the field did not exist and a role's search ran over every tier.
func TestMemorySearch618_RoleCapReachesTheSearch(t *testing.T) {
	stub := &policyStub{}
	pol := &registry.MemorySearchPolicy{RepoScope: policyScope, StrictScope: true, Authority: "provenance", Limit: 8, MaxSensitivity: "public"}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) { return pol, nil }))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x&max_sensitivity=restricted&maxSensitivity=confidential"))
	decodePolicyResp(t, rec)
	if got := stub.calls[0].MaxSensitivity; got != "public" {
		t.Fatalf("search got cap %q, want the role's public", got)
	}
	if !strings.Contains(rec.Body.String(), `"max_sensitivity":"public"`) {
		t.Fatalf("the retrieval_policy block must name the cap:\n%s", rec.Body.String())
	}
}

// An operator's search (no role policy) carries no cap: the query string
// cannot introduce one.
func TestMemorySearch618_QueryStringSetsNoCap(t *testing.T) {
	req, ok := memorySearchRequestFrom(httptest.NewRequest("GET", "/api/v1/projects/p1/memory/search?q=x&max_sensitivity=public", nil), 5)
	if !ok || req.MaxSensitivity != "" {
		t.Fatalf("the query string must not set a cap: %+v", req)
	}
}

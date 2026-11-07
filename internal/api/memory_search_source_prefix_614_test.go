package api

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// #614 C3: the role's sourcePrefixes reach the search for the role's own
// key, the query string cannot set or widen them, and the denominator block
// names them. Before the change the field did not exist and the search ran
// over every source.
func TestMemorySearch614_RolePrefixesReachTheSearch(t *testing.T) {
	stub := &policyStub{}
	prefixes := []string{"docs/architect/", "docs/public/"}
	pol := &registry.MemorySearchPolicy{RepoScope: policyScope, StrictScope: true, Authority: "provenance", Limit: 8, SourcePrefixes: prefixes}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) { return pol, nil }))
	rec := httptest.NewRecorder()
	// A hand-built request tries to name its own prefixes; the query string
	// has no such parameter, and the role's list stands.
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x&source_prefixes=https://docs.vornik.io&source_prefix=docs/legal/"))
	decodePolicyResp(t, rec)
	if got := stub.calls[0].SourcePrefixes; !reflect.DeepEqual(got, prefixes) {
		t.Fatalf("search got prefixes %v, want the role's %v", got, prefixes)
	}
	if !strings.Contains(rec.Body.String(), `"source_prefixes":["docs/architect/","docs/public/"]`) {
		t.Fatalf("the retrieval_policy block must name the prefixes:\n%s", rec.Body.String())
	}
}

// An operator's search (no role policy) carries no prefixes: the query
// string cannot introduce them.
func TestMemorySearch614_QueryStringSetsNoPrefixes(t *testing.T) {
	req, ok := memorySearchRequestFrom(httptest.NewRequest("GET", "/api/v1/projects/p1/memory/search?q=x&source_prefixes=docs/public/", nil), 5)
	if !ok || req.SourcePrefixes != nil {
		t.Fatalf("the query string must not set prefixes: %+v", req)
	}
}

package api

// GitHub #550 (found at: production, 2026-10-25). Design:
// https://docs.vornik.io §2.1 C1. The
// REST memory search takes repo_scope / strict_scope / authority, and a
// request made with an executor task key gets its role's pinned policy,
// which a wider query parameter cannot loosen.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// policyStub records every routing search it serves.
type policyStub struct {
	calls   []MemorySearchRequest
	results func(req MemorySearchRequest) []MemorySearchResult
}

func (p *policyStub) Search(context.Context, string, string, int) ([]MemorySearchResult, error) {
	return nil, errors.New("the policy path must use SearchRouting")
}

func (p *policyStub) SearchRouting(_ context.Context, _, _ string, req MemorySearchRequest) ([]MemorySearchResult, *RoutingVerdictWire, error) {
	p.calls = append(p.calls, req)
	if p.results == nil {
		return nil, nil, nil
	}
	return p.results(req), nil, nil
}

const policyScope = "github.com/EaseIT-cz/vornik"

// executorKeyReq authenticates as the per-task key the executor mints for
// task t1.
func executorKeyReq(target string) *http.Request {
	const taskID = "t1"
	r := httptest.NewRequest(http.MethodGet, target, nil)
	row := &persistence.APIKey{Name: persistence.TaskKeyNamePrefix + taskID, ProjectID: "p1", CreatedBy: executorKeyCreator}
	ctx := context.WithValue(r.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}})
	return r.WithContext(ctx)
}

func decodePolicyResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMemorySearch550_QueryParamsReachTheSearch(t *testing.T) {
	stub := &policyStub{}
	srv := NewServer(WithMemorySearcher(stub))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/p1/memory/search?q=x&limit=7&repo_scope="+policyScope+"&strict_scope=true&authority=provenance", nil))
	decodePolicyResp(t, rec)
	got := stub.calls[0]
	want := MemorySearchRequest{Limit: 7, RepoScope: policyScope, StrictScope: true, Authority: "provenance"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search got %+v, want %+v", got, want)
	}
}

func TestMemorySearch550_UnknownAuthorityIsRefused(t *testing.T) {
	srv := NewServer(WithMemorySearcher(&policyStub{}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x&authority=role-of-record", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestMemorySearch550_RolePolicyOverridesAWiderRequest(t *testing.T) {
	stub := &policyStub{}
	pol := &registry.MemorySearchPolicy{RepoScope: policyScope, StrictScope: true, Authority: "provenance", Limit: 8}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(_ context.Context, who AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		if who.TaskID != "t1" {
			t.Errorf("resolver asked for %+v", who)
		}
		return pol, nil
	}))
	rec := httptest.NewRecorder()
	// Every parameter asks for more than the role may have.
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x&limit=50&repo_scope=github.com/other/repo&strict_scope=false&authority=any"))
	decodePolicyResp(t, rec)
	want := MemorySearchRequest{Limit: 8, RepoScope: policyScope, StrictScope: true, Authority: "provenance"}
	if got := stub.calls[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("search got %+v, want the role policy %+v", got, want)
	}
}

func TestMemorySearch550_RolePolicyKeepsANarrowerRequest(t *testing.T) {
	stub := &policyStub{}
	pol := &registry.MemorySearchPolicy{RepoScope: policyScope, Limit: 8}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) { return pol, nil }))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x&limit=3&strict_scope=true&authority=provenance"))
	decodePolicyResp(t, rec)
	want := MemorySearchRequest{Limit: 3, RepoScope: policyScope, StrictScope: true, Authority: "provenance"}
	if got := stub.calls[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("search got %+v, want %+v", got, want)
	}
}

func TestMemorySearch550_PolicyDefaultLimitWhenTheCallNamesNone(t *testing.T) {
	stub := &policyStub{}
	pol := &registry.MemorySearchPolicy{RepoScope: policyScope, Limit: 8}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) { return pol, nil }))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x"))
	decodePolicyResp(t, rec)
	if got := stub.calls[0].Limit; got != 8 {
		t.Fatalf("limit %d, want the policy default 8", got)
	}
}

func TestMemorySearch550_UnresolvableTaskPolicyFailsClosed(t *testing.T) {
	stub := &policyStub{}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		return nil, errors.New("no execution for this task")
	}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x"))
	if rec.Code != http.StatusServiceUnavailable || len(stub.calls) != 0 {
		t.Fatalf("status %d after %d searches; want 503 and no search", rec.Code, len(stub.calls))
	}
}

func TestMemorySearch550_NonExecutorKeyIsNotGivenARolePolicy(t *testing.T) {
	stub := &policyStub{}
	called := false
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		called = true
		return &registry.MemorySearchPolicy{RepoScope: policyScope, Authority: "provenance"}, nil
	}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x", nil))
	decodePolicyResp(t, rec)
	if called || !reflect.DeepEqual(stub.calls[0], MemorySearchRequest{Limit: 10}) {
		t.Fatalf("resolver called=%v, search %+v; an operator request keeps its own parameters", called, stub.calls[0])
	}
}

func TestMemorySearch550_NarrowingWithoutRoutingSearcherFailsClosed(t *testing.T) {
	plain := &stubMemorySearcher{searchFn: func(context.Context, string, string, int) ([]MemorySearchResult, error) {
		return []MemorySearchResult{{ChunkID: "review"}}, nil
	}}
	srv := NewServer(WithMemorySearcher(plain))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x&repo_scope="+policyScope+"&authority=provenance", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d; a searcher that cannot filter must refuse a filtered search, not ignore the filter", rec.Code)
	}
}

func TestMemorySearch550_DenominatorTellsExcludedFromAbsent(t *testing.T) {
	stub := &policyStub{results: func(req MemorySearchRequest) []MemorySearchResult {
		if req.Authority == "provenance" {
			return nil // the policy excluded everything
		}
		return []MemorySearchResult{{ChunkID: "review"}, {ChunkID: "note"}}
	}}
	srv := NewServer(WithMemorySearcher(stub))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x&limit=5&repo_scope="+policyScope+"&authority=provenance", nil))
	m := decodePolicyResp(t, rec)
	var pol struct {
		RepoScope          string `json:"repo_scope"`
		Authority          string `json:"authority"`
		Returned           int    `json:"returned"`
		UnfilteredReturned *int   `json:"unfiltered_returned"`
	}
	if err := json.Unmarshal(m["retrieval_policy"], &pol); err != nil {
		t.Fatalf("retrieval_policy: %v (body %s)", err, rec.Body.String())
	}
	if pol.Returned != 0 || pol.UnfilteredReturned == nil || *pol.UnfilteredReturned != 2 || pol.Authority != "provenance" || pol.RepoScope != policyScope {
		t.Fatalf("retrieval_policy %+v", pol)
	}
	if len(stub.calls) != 2 || !reflect.DeepEqual(stub.calls[1], MemorySearchRequest{Limit: 5}) {
		t.Fatalf("calls %+v; want the filtered search, then one unfiltered count", stub.calls)
	}
}

func TestMemorySearch550_UnfilteredSearchHasNoPolicyBlock(t *testing.T) {
	srv := NewServer(WithMemorySearcher(&policyStub{}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x", nil))
	if _, ok := decodePolicyResp(t, rec)["retrieval_policy"]; ok {
		t.Fatal("an unfiltered search must keep today's response shape")
	}
}

// An ad-hoc request may ask for authority alone: "a policy names its
// repository" is a loader rule for ROLE policies (registry), not for a
// caller's own search, and the SQL keeps an authority-only search to
// provenance-checked chunks across every scope (review 8446 F1, behaviour
// pinned rather than changed).
func TestMemorySearch550_AuthorityWithoutScopeReachesTheSearch(t *testing.T) {
	stub := &policyStub{}
	srv := NewServer(WithMemorySearcher(stub))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x&authority=provenance", nil))
	decodePolicyResp(t, rec)
	if got := stub.calls[0]; !reflect.DeepEqual(got, MemorySearchRequest{Limit: 10, Authority: "provenance"}) {
		t.Fatalf("search got %+v", got)
	}
}

// warmKeyReq authenticates as the warm-pool key the executor mints for a
// pooled role (agent:warm_<project>:<role>).
func warmKeyReq(target, name string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	row := &persistence.APIKey{Name: name, ProjectID: "p1", CreatedBy: executorKeyCreator}
	ctx := context.WithValue(r.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}})
	return r.WithContext(ctx)
}

// Security review 2026-10-25 (finding 1): a role on a warm container
// authenticates with the warm-pool key, not the per-task key. Before the fix
// that key was not recognised, so a warm role's search ran unfiltered.
func TestMemorySearch550_WarmPoolKeyGetsItsRolePolicy(t *testing.T) {
	stub := &policyStub{}
	var asked AgentKeyPrincipal
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(_ context.Context, who AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		asked = who
		return &registry.MemorySearchPolicy{RepoScope: policyScope, Authority: "provenance", Limit: 8}, nil
	}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, warmKeyReq("/api/v1/projects/p1/memory/search?q=x&authority=any", "agent:warm_p1:consultant"))
	decodePolicyResp(t, rec)
	if asked != (AgentKeyPrincipal{ProjectID: "p1", Role: "consultant"}) {
		t.Fatalf("resolver asked for %+v", asked)
	}
	if got := stub.calls[0]; got.Authority != "provenance" || got.RepoScope != policyScope {
		t.Fatalf("SECURITY: a warm-pool key searched without its role policy: %+v", got)
	}
}

// A key that looks like an agent key but names no task and no role of its own
// project resolves to nothing, and is refused rather than served unfiltered.
func TestMemorySearch550_MalformedAgentKeyIsRefused(t *testing.T) {
	for _, name := range []string{"agent:warm_other-project:consultant", "agent:warm_p1:", "agent:warm_p1"} {
		t.Run(name, func(t *testing.T) {
			stub := &policyStub{}
			srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(_ context.Context, who AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
				if who.Role == "" && who.TaskID == "" {
					return nil, errors.New("no task and no role")
				}
				return nil, nil
			}))
			rec := httptest.NewRecorder()
			srv.MemorySearch(rec, warmKeyReq("/api/v1/projects/p1/memory/search?q=x", name))
			if rec.Code != http.StatusServiceUnavailable || len(stub.calls) != 0 {
				t.Fatalf("status %d after %d searches; want 503 and no search", rec.Code, len(stub.calls))
			}
		})
	}
}

// Security review 2026-10-25 (finding 2): for an agent key the denominator
// runs no second, unfiltered search. It would tell the model how much its
// policy excluded and record a retrieval of chunks it never saw.
func TestMemorySearch550_AgentKeyDenominatorRunsNoUnfilteredSearch(t *testing.T) {
	stub := &policyStub{results: func(MemorySearchRequest) []MemorySearchResult { return nil }}
	srv := NewServer(WithMemorySearcher(stub), WithMemorySearchRolePolicy(func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		return &registry.MemorySearchPolicy{RepoScope: policyScope, Authority: "provenance", Limit: 8}, nil
	}))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, executorKeyReq("/api/v1/projects/p1/memory/search?q=x"))
	m := decodePolicyResp(t, rec)
	var pol map[string]json.RawMessage
	if err := json.Unmarshal(m["retrieval_policy"], &pol); err != nil {
		t.Fatalf("retrieval_policy: %v", err)
	}
	if _, leaked := pol["unfiltered_returned"]; leaked || len(stub.calls) != 1 {
		t.Fatalf("calls %d, policy block %s; want one search and no unfiltered count", len(stub.calls), m["retrieval_policy"])
	}
}

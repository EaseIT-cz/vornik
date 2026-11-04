package api

// GitHub #613 (P1 security): a plain database key (no client kind, not an
// agent key: what `vornikctl key create` mints) was a full key on every route
// of its project, whatever its memory_read, memory_write and
// allowed_workflows said. A key handed to an A2A caller, narrowed to one
// workflow with every flag false, read the project's private memory, listed
// its keys and could mint a new one. Found at: production (reproduced against
// the live daemon, 2026-11-01). Regression test at: integration, on the #467
// rig (the production router over real SQLite, auth on). Lower because: the
// HTTP e2e lane boots its daemon with auth off, so no key is confined there,
// and the refusal happens in the middleware before any handler runs.
// Design: 18-security-and-tenancy.md, Amendment 2026-11-01 (#613).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskkeys"
)

const (
	static613   = "sk-vornik-operator-613static0123456789abcdef"
	workflow613 = "wf-613"
)

type rig613 struct {
	h         http.Handler
	agentKeys map[string]string // per-task and warm-pool agent keys
	narrowed  string            // plain key, allowed_workflows = [workflow613], no flags
	noMemory  string            // plain key, no list, no flags
	reader    string            // plain key, memory_read
	writer    string            // plain key, memory_read + memory_write
	companion string            // client_kind claude-code, memory_read
}

// newRig613 is the production router (AuthMiddleware -> the real handlers)
// over real SQLite, auth on, with a memory searcher wired so a request that
// passes the middleware is answered 200, not 503.
func newRig613(t *testing.T) rig613 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: tcTaskID, ProjectID: tcProjectID, Status: persistence.TaskStatusCompleted, MaxAttempts: 3, Attempt: 1,
		Payload: []byte(`{}`),
	}))
	repo := sqlite.NewAPIKeyRepository(db.DB)
	mint := func(k persistence.APIKey) string {
		raw, err := apikey.Generate(tcProjectID)
		require.NoError(t, err)
		k.ID = persistence.GenerateID("akey")
		k.ProjectID = tcProjectID
		k.KeyHash, k.KeyPrefix = apikey.Hash(raw), apikey.DisplayPrefix(raw)
		if k.CreatedBy == "" {
			k.CreatedBy = "static"
		}
		k.CreatedAt = time.Now().UTC()
		require.NoError(t, repo.Create(ctx, &k))
		return raw
	}
	stub := &policyStub{results: func(MemorySearchRequest) []MemorySearchResult {
		return []MemorySearchResult{{ChunkID: "chunk-1"}}
	}}
	noPolicy := func(context.Context, AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) { return nil, nil }
	srv := NewServer(
		WithTaskRepository(tasks),
		WithExecutionRepository(sqlite.NewExecutionRepository(db.DB)),
		WithTaskMessageRepository(sqlite.NewTaskMessageRepository(db.DB)),
		WithAPIKeyRepository(repo),
		WithMemorySearcher(stub),
		WithMemorySearchRolePolicy(noPolicy),
		// The admin surface as an Enterprise daemon wires it, so admin-gated
		// routes answer from their own gate, not 501 (round 3).
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{static613}}),
	)
	srv.logger = zerolog.Nop()
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{static613}
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{static613}}
	r := rig613{agentKeys: map[string]string{}}
	r.agentKeys["task"] = mint(persistence.APIKey{Name: persistence.TaskKeyNamePrefix + tcTaskID, CreatedBy: executorKeyCreator})
	r.agentKeys["warm"] = mint(persistence.APIKey{Name: taskkeys.WarmAgentKeyNamePrefix + tcProjectID + ":lead", CreatedBy: executorKeyCreator})
	r.narrowed = mint(persistence.APIKey{Name: "a2a caller", AllowedWorkflows: []string{workflow613}})
	r.noMemory = mint(persistence.APIKey{Name: "script"})
	r.reader = mint(persistence.APIKey{Name: "reader", MemoryRead: true})
	r.writer = mint(persistence.APIKey{Name: "writer", MemoryRead: true, MemoryWrite: true})
	r.companion = mint(persistence.APIKey{Name: "companion", ClientKind: "claude-code", MemoryRead: true})
	r.h = recoverPanic467(NewRouter(srv, cfg).Handler())
	return r
}

func (r rig613) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func errCode613(rec *httptest.ResponseRecorder) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error.Code
}

func p613(rest string) string { return "/api/v1/projects/" + tcProjectID + rest }

// memoryReadRoutes613 and memoryWriteRoutes613 are the C2 routes; the census
// below fails when routes.go grows one that is not classified.
var memoryReadRoutes613 = []string{
	"/memory/search?q=x", "/memory/feedback", "/memory/epochs", "/memory/rollbacks",
	"/memory/health", "/memory/quarantine", "/memory/search/?q=x",
}

var memoryWriteRoutes613 = []string{
	"/memory/rollback", "/memory/quarantine/chunk-1/release", "/artifacts/art-1/extract",
}

var keyRoutes613 = []struct{ method, path string }{
	{http.MethodGet, "/keys"},
	{http.MethodPost, "/keys"},
	{http.MethodGet, "/keys/"},
	{http.MethodPost, "/keys/akey_x/rotate"},
	{http.MethodPut, "/keys/akey_x/workflows"},
	{http.MethodPut, "/keys/akey_x/allow-push"},
	{http.MethodDelete, "/keys/akey_x"},
}

func TestPlainKey613_WorkflowListedKeyReachesOnlyA2A(t *testing.T) {
	r := newRig613(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, p613("/memory/search?q=x")},
		{http.MethodGet, p613("/tasks")},
		{http.MethodGet, p613("/tasks/" + tcTaskID)},
		{http.MethodGet, p613("/tasks/" + tcTaskID + "/messages")},
		{http.MethodGet, p613("/keys")},
		{http.MethodPost, p613("/keys")},
		{http.MethodGet, p613("/executions")},
		{http.MethodGet, p613("/config")},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodGet, p613("//memory/search?q=x")},
		{http.MethodGet, "/a2a/v1/agents/" + tcProjectID + "/other-wf/tasks"},
		{http.MethodGet, "/a2a/v1/agents/" + tcProjectID + "/" + workflow613 + "/../other-wf/tasks"},
		{http.MethodHead, p613("/memory/search?q=x")},
	} {
		rec := r.do(c.method, c.path, r.narrowed, "")
		require.Equalf(t, http.StatusForbidden, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		require.Equalf(t, "KEY_WORKFLOW_SCOPED", errCode613(rec), "%s %s", c.method, c.path)
	}
}

func TestPlainKey613_WorkflowListedKeyStillReachesItsA2AWorkflow(t *testing.T) {
	r := newRig613(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/a2a/v1/agents/" + tcProjectID + "/" + workflow613 + "/tasks"},
		{http.MethodGet, "/a2a/v1/agents/" + tcProjectID + "/" + workflow613 + "/tasks/t1"},
	} {
		rec := r.do(c.method, c.path, r.narrowed, `{}`)
		require.NotEqualf(t, "KEY_WORKFLOW_SCOPED", errCode613(rec), "%s %s: %s", c.method, c.path, rec.Body.String())
		require.NotEqualf(t, http.StatusUnauthorized, rec.Code, "%s %s", c.method, c.path)
	}
}

func TestPlainKey613_MemoryRoutesNeedTheFlags(t *testing.T) {
	r := newRig613(t)
	for _, route := range memoryReadRoutes613 {
		rec := r.do(http.MethodGet, p613(route), r.noMemory, "")
		require.Equalf(t, http.StatusForbidden, rec.Code, "GET %s without memory_read: %s", route, rec.Body.String())
		require.Equalf(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "GET %s", route)
		require.Containsf(t, rec.Body.String(), "memory_read", "GET %s names the flag", route)

		rec = r.do(http.MethodGet, p613(route), r.reader, "")
		require.NotEqualf(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "GET %s with memory_read", route)
	}
	for _, route := range memoryWriteRoutes613 {
		for _, key := range []string{r.noMemory, r.reader} {
			rec := r.do(http.MethodPost, p613(route), key, `{}`)
			require.Equalf(t, http.StatusForbidden, rec.Code, "POST %s without memory_write: %s", route, rec.Body.String())
			require.Equalf(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "POST %s", route)
			require.Containsf(t, rec.Body.String(), "memory_write", "POST %s names the flag", route)
		}
		rec := r.do(http.MethodPost, p613(route), r.writer, `{}`)
		require.NotEqualf(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "POST %s with memory_write", route)
	}
}

func TestPlainKey613_DatabaseKeyCannotManageKeys(t *testing.T) {
	r := newRig613(t)
	for _, key := range []string{r.noMemory, r.writer} {
		for _, c := range keyRoutes613 {
			rec := r.do(c.method, p613(c.path), key, `{"name":"x"}`)
			require.Equalf(t, http.StatusForbidden, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
			require.Equalf(t, "KEY_MANAGEMENT_OPERATOR_ONLY", errCode613(rec), "%s %s", c.method, c.path)
		}
	}
	rec := r.do(http.MethodGet, p613("/keys"), static613, "")
	require.Equal(t, http.StatusOK, rec.Code, "a static key still lists keys: %s", rec.Body.String())
	rec = r.do(http.MethodPost, p613("/keys"), static613, `{"name":"from static"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "a static key still creates keys: %s", rec.Body.String())
}

func TestPlainKey613_OtherCallersUnchanged(t *testing.T) {
	r := newRig613(t)
	// A full plain key keeps the submitter routes of its project (§4).
	rec := r.do(http.MethodGet, p613("/tasks"), r.noMemory, "")
	require.Equal(t, http.StatusOK, rec.Code, "plain key lists tasks: %s", rec.Body.String())
	// A static key is not a database key: no C1-C3 refusal anywhere.
	for _, route := range append(append([]string{}, memoryReadRoutes613...), "/tasks", "/keys") {
		rec := r.do(http.MethodGet, p613(route), static613, "")
		require.NotContainsf(t, []string{"KEY_WORKFLOW_SCOPED", "MEMORY_CAPABILITY_REQUIRED", "KEY_MANAGEMENT_OPERATOR_ONLY"},
			errCode613(rec), "static key on %s", route)
	}
	// An agent key keeps memory search (the role policy governs it, #550).
	for name, raw := range r.agentKeys {
		rec := r.do(http.MethodGet, p613("/memory/search?q=x"), raw, "")
		require.Equalf(t, http.StatusOK, rec.Code, "agent key %s on memory search: %s", name, rec.Body.String())
	}
	// A plain key with memory_read is answered by the searcher.
	rec = r.do(http.MethodGet, p613("/memory/search?q=x"), r.reader, "")
	require.Equal(t, http.StatusOK, rec.Code, "reader on memory search: %s", rec.Body.String())
	// A companion key keeps its own confinement and message.
	rec = r.do(http.MethodGet, p613("/memory/search?q=x"), r.companion, "")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "companion API keys may only use")
}

// TestPlainKey613_CensusEveryMemoryRouteIsClassified parses the project
// dispatcher and fails when a /memory/ route, or a route ending in /extract,
// is one plainKeyRefusal does not refuse to a key without the flags.
func TestPlainKey613_CensusEveryMemoryRouteIsClassified(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	require.NoError(t, err)
	exact := regexp.MustCompile(`remaining == "(/memory/[^"]*)"`)
	prefix := regexp.MustCompile(`strings\.HasPrefix\(remaining, "(/memory/[^"]*)"\)`)
	extract := regexp.MustCompile(`strings\.HasSuffix\(remaining, "(/extract)"\)`)
	var routes []string
	for _, m := range exact.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1])
	}
	for _, m := range prefix.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1]+"x")
	}
	for range extract.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, "/artifacts/a/extract")
	}
	require.GreaterOrEqual(t, len(routes), 10, "the census found too few routes; the dispatcher's shape changed")
	bare := &persistence.APIKey{Name: "script"}
	for _, route := range routes {
		route = strings.TrimSuffix(route, "/")
		path := "/api/v1/projects/p" + route
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			code, _ := plainKeyRefusal(bare, method, path)
			if strings.HasSuffix(route, "/extract") && method == http.MethodGet {
				continue
			}
			require.Equalf(t, "MEMORY_CAPABILITY_REQUIRED", code, "%s %s is not classified", method, path)
		}
	}
}

// TestPlainKey613_PathVariantsCannotStepAround pins the review finding the
// design review got wrong (review-20261101-3fc8 assumed Go normalises the path
// before the middleware; the middleware wraps the mux and sees it raw).
func TestPlainKey613_PathVariantsCannotStepAround(t *testing.T) {
	bare := &persistence.APIKey{Name: "script", ProjectID: "p"}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/api/v1/projects/p//memory/search", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, "/api/v1/projects/p/./memory/search", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, "/api/v1/projects/p/tasks/../memory/search", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, "/api/v1/projects/p/memory/search/", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodHead, "/api/v1/projects/p/memory/search", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodOptions, "/api/v1/projects/p/memory/search", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, "/api/v1/projects/p//keys", "KEY_MANAGEMENT_OPERATOR_ONLY"},
		{http.MethodGet, "/api/v1/projects/p/keys/", "KEY_MANAGEMENT_OPERATOR_ONLY"},
		{http.MethodPost, "/api/v1/projects/p/artifacts/a//extract", "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, "/api/v1/projects/p/tasks", ""},
		{http.MethodGet, "/api/v1/projects/p", ""},
	} {
		code, _ := plainKeyRefusal(bare, c.method, c.path)
		require.Equalf(t, c.want, code, "%s %s", c.method, c.path)
	}
	reader := &persistence.APIKey{Name: "reader", ProjectID: "p", MemoryRead: true}
	code, _ := plainKeyRefusal(reader, http.MethodPost, "/api/v1/projects/p/memory/rollback")
	require.Equal(t, "MEMORY_CAPABILITY_REQUIRED", code, "memory_read alone does not write")
	narrowed := &persistence.APIKey{Name: "a2a", ProjectID: "p", AllowedWorkflows: []string{"wf"}}
	for _, p := range []string{"/a2a/v1/agents/p/wf", "/a2a/v1/agents/p/wf/tasks", "/a2a/v1/agents/p/wf/tasks/t1"} {
		code, _ := plainKeyRefusal(narrowed, http.MethodPost, p)
		require.Equalf(t, "", code, "%s is its own A2A workflow", p)
	}
	for _, p := range []string{"/a2a/v1/agents/p/wf/", "/a2a/v1/agents//p/wf/tasks", "/a2a/v1/agents/q/wf/tasks", "/a2a/v1/agents/p/wf2/tasks", "/a2a/v1/agents/p"} {
		code, _ := plainKeyRefusal(narrowed, http.MethodPost, p)
		require.Equalf(t, "KEY_WORKFLOW_SCOPED", code, "%s", p)
	}
}

// Round 2 (automated security review of 8264a3511, found at: review; observed
// at the production HTTP surface): three routes outside the project memory
// and keys trees let a plain key with no flags mint a memory-capable companion
// key, rewrite memory metadata, or read the memory-derived gist.
// Design: 18-security-and-tenancy.md, Amendment 2026-11-01 (#613), Round 2.
func TestPlainKey613_Round2_OtherMintAndMemoryRoutes(t *testing.T) {
	r := newRig613(t)
	for _, c := range []struct{ method, path, body, want string }{
		{http.MethodPost, "/api/v1/companion/grant", `{"project_id":"` + tcProjectID + `","client_kind":"claude-code","memory_read":true,"memory_write":true}`, "KEY_MANAGEMENT_OPERATOR_ONLY"},
		{http.MethodGet, "/api/v1/companion/keys?project_id=" + tcProjectID, "", "KEY_MANAGEMENT_OPERATOR_ONLY"},
		{http.MethodPost, "/api/v1/memory/reclassify-llm?project=" + tcProjectID, `{}`, "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodPost, "/api/v1/memory/regraph?project=" + tcProjectID, `{}`, "MEMORY_CAPABILITY_REQUIRED"},
		// Round 3: with ?project= the quarantine re-flag accepts a project key
		// (automated security review of 1fc0fe379; round 2 read only the
		// no-project branch and filed it as operator-gated).
		{http.MethodPost, "/api/v1/memory/regraph/quarantined?project=" + tcProjectID, `{}`, "MEMORY_CAPABILITY_REQUIRED"},
		{http.MethodGet, p613("/gist"), "", "MEMORY_CAPABILITY_REQUIRED"},
	} {
		rec := r.do(c.method, c.path, r.noMemory, c.body)
		require.Equalf(t, http.StatusForbidden, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		require.Equalf(t, c.want, errCode613(rec), "%s %s", c.method, c.path)
	}
	// The flags still open what they govern.
	rec := r.do(http.MethodGet, p613("/gist"), r.reader, "")
	require.NotEqual(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "reader on gist: %s", rec.Body.String())
	rec = r.do(http.MethodPost, "/api/v1/memory/regraph?project="+tcProjectID, r.writer, `{}`)
	require.NotEqual(t, "MEMORY_CAPABILITY_REQUIRED", errCode613(rec), "writer on regraph: %s", rec.Body.String())
	// A static key still mints companion keys (vornikctl companion grant).
	rec = r.do(http.MethodGet, "/api/v1/companion/keys?project_id="+tcProjectID, static613, "")
	require.NotEqual(t, "KEY_MANAGEMENT_OPERATOR_ONLY", errCode613(rec), "static key on companion keys: %s", rec.Body.String())
}

// TestPlainKey613_Round2_CensusDaemonMemoryRoutes parses the daemon-wide
// /api/v1/memory/ registrations and requires each to be classified, and pins
// the memory-reading handlers outside memory/ by name.
func TestPlainKey613_Round2_CensusDaemonMemoryRoutes(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	require.NoError(t, err)
	reg := regexp.MustCompile(`mux\.HandleFunc\("(/api/v1/memory/[^"]*)"`)
	found := reg.FindAllStringSubmatch(string(src), -1)
	require.GreaterOrEqual(t, len(found), 11, "the census found too few /api/v1/memory/ registrations")
	// Operator surface under /api/v1/memory/: their own gate answers a
	// project-scoped key (404 for the #495 set), so plainKeyRefusal must leave
	// them alone. A new registration goes in this list or memoryDaemonRoutes.
	operatorGated := map[string]bool{
		"/api/v1/memory/policy/evaluations":         true,
		"/api/v1/memory/policy/evaluations.csv":     true,
		"/api/v1/memory/policy/evaluations/digest/": true,
		"/api/v1/memory/policy/mode":                true,
		"/api/v1/memory/policy/chunks/":             true,
		"/api/v1/memory/cache-stats":                true,
		"/api/v1/memory/backfill-titles":            true,
	}
	bare := &persistence.APIKey{Name: "script", ProjectID: "p"}
	for _, m := range found {
		route := m[1]
		require.Truef(t, memoryDaemonRoutes[route] != operatorGated[route],
			"%s must be in exactly one of memoryDaemonRoutes or the operator-gated list", route)
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			code, _ := plainKeyRefusal(bare, method, route)
			if memoryDaemonRoutes[route] {
				require.Equalf(t, "MEMORY_CAPABILITY_REQUIRED", code, "%s %s", method, route)
			} else {
				require.Equalf(t, "", code, "%s %s is operator surface; its own gate answers", method, route)
			}
		}
	}
	// Outside memory/, the handlers that read or write memory (census of the
	// 56 project-dispatcher handlers, 2026-11-01). A new one is added here and
	// to plainKeyRefusal together.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/projects/p/artifacts/a/extract"},
		{http.MethodGet, "/api/v1/projects/p/gist"},
	} {
		code, _ := plainKeyRefusal(bare, c.method, c.path)
		require.Equalf(t, "MEMORY_CAPABILITY_REQUIRED", code, "%s %s", c.method, c.path)
	}
	for _, p := range []string{"/api/v1/companion/grant", "/api/v1/companion/keys", "/api/v1/companion//grant", "/api/v1/companion/keys/"} {
		code, _ := plainKeyRefusal(bare, http.MethodPost, p)
		require.Equalf(t, "KEY_MANAGEMENT_OPERATOR_ONLY", code, "%s", p)
	}
}

// TestPlainKey613_Round3_OperatorGatedRoutesRefuseAPlainKey proves the
// operator-gated list instead of trusting it: through the production router,
// a plain key with every flag must be refused by each route's own gate (round
// 2 mislabelled regraph/quarantined, which accepts a project key with
// ?project=; automated security review of 1fc0fe379).
func TestPlainKey613_Round3_OperatorGatedRoutesRefuseAPlainKey(t *testing.T) {
	r := newRig613(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/memory/policy/evaluations?project_id=" + tcProjectID},
		{http.MethodGet, "/api/v1/memory/policy/evaluations.csv?project_id=" + tcProjectID},
		{http.MethodGet, "/api/v1/memory/policy/evaluations/digest/d1"},
		{http.MethodGet, "/api/v1/memory/policy/mode"},
		{http.MethodPost, "/api/v1/memory/policy/chunks/c1"},
		{http.MethodGet, "/api/v1/memory/cache-stats"},
		{http.MethodPost, "/api/v1/memory/backfill-titles?project=" + tcProjectID},
	} {
		rec := r.do(c.method, c.path, r.writer, `{}`)
		require.Containsf(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code,
			"%s %s must be refused by its own gate: %d %s", c.method, c.path, rec.Code, rec.Body.String())
	}
}

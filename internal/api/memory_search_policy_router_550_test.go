package api

// GitHub #550 (found at: production, the operator's question of 2026-10-25,
// compared as e2e-lane). Design:
// https://docs.vornik.io §2.5.
//
// Regression test at: integration, the production router (NewRouter:
// AuthMiddleware -> ProjectAuthMiddleware -> MemorySearch) over real SQLite
// with a really minted executor task key. Lower because: the property is
// which search the daemon runs for a task key's request, whatever that
// request asks; it is decided here, before any container or model takes
// part. The executor's only share (the VORNIK_MEM_* env) is unit-tested and
// the container side is shell-tested (test-entrypoint-memory-search-policy.sh);
// the search SQL is integration-tested on Postgres
// (search_authority_550_integration_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

type rig550 struct {
	h        http.Handler
	taskKey  string
	warmKey  string // the warm-pool key of the consultant role
	operator string
	stub     *policyStub
}

func newRig550(t *testing.T) rig550 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: tcTaskID, ProjectID: tcProjectID, Status: persistence.TaskStatusRunning, MaxAttempts: 3, Attempt: 1,
		Payload: []byte(`{}`),
	}))
	execs := sqlite.NewExecutionRepository(db.DB)
	step := "answer"
	require.NoError(t, execs.Create(ctx, &persistence.Execution{
		ID: "exec-550", TaskID: tcTaskID, ProjectID: tcProjectID, WorkflowID: "vornik-architecture",
		CurrentStepID: &step, Status: persistence.ExecutionStatusRunning, CreatedAt: time.Now().UTC(),
	}))
	keyRepo := sqlite.NewAPIKeyRepository(db.DB)
	mint := func(name, createdBy string) string {
		raw, err := apikey.Generate(tcProjectID)
		require.NoError(t, err)
		require.NoError(t, keyRepo.Create(ctx, &persistence.APIKey{
			ID: persistence.GenerateID("akey"), ProjectID: tcProjectID, Name: name,
			KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw),
			CreatedBy: createdBy, CreatedAt: time.Now().UTC(),
		}))
		return raw
	}
	stub := &policyStub{results: func(req MemorySearchRequest) []MemorySearchResult {
		// The design is the only provenance-checked hit; the review and the
		// note are what an unfiltered search adds.
		hits := []MemorySearchResult{{ChunkID: "design"}}
		if req.Authority != "provenance" {
			hits = append(hits, MemorySearchResult{ChunkID: "review"}, MemorySearchResult{ChunkID: "note"})
		}
		return hits
	}}
	// The resolver reads the real execution row, as the service's does.
	consultant := &registry.MemorySearchPolicy{RepoScope: "github.com/EaseIT-cz/vornik", StrictScope: true, Authority: "provenance", Limit: 8}
	resolver := func(ctx context.Context, who AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
		if who.TaskID == "" {
			// A warm-pool key: the role named in it, in its own project.
			if who.ProjectID == tcProjectID && who.Role == "consultant" {
				return consultant, nil
			}
			return nil, errors.New("no such role in the project")
		}
		exec, err := execs.GetByTaskID(ctx, who.TaskID)
		if err != nil || exec == nil || exec.CurrentStepID == nil || *exec.CurrentStepID != "answer" {
			return nil, err
		}
		return &registry.MemorySearchPolicy{RepoScope: "github.com/EaseIT-cz/vornik", StrictScope: true, Authority: "provenance", Limit: 8}, nil
	}
	srv := NewServer(
		WithTaskRepository(tasks),
		WithExecutionRepository(execs),
		WithAPIKeyRepository(keyRepo),
		WithMemorySearcher(stub),
		WithMemorySearchRolePolicy(resolver),
	)
	srv.logger = zerolog.Nop()
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	return rig550{
		h:        recoverPanic467(NewRouter(srv, cfg).Handler()),
		taskKey:  mint(persistence.TaskKeyNamePrefix+tcTaskID, executorKeyCreator),
		warmKey:  mint(taskkeys.WarmAgentKeyNamePrefix+tcProjectID+":consultant", executorKeyCreator),
		operator: mint("operator laptop", "operator"),
		stub:     stub,
	}
}

func (r rig550) search(t *testing.T, bearer, query string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+tcProjectID+"/memory/search?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp memorySearchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Results))
	for _, h := range resp.Results {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

func TestMemorySearch550_TaskKeyThroughTheRouterGetsOnlyTheCheckedDesign(t *testing.T) {
	r := newRig550(t)
	// What the agent sends with its env policy.
	require.Equal(t, []string{"design"}, r.search(t, r.taskKey,
		"q=spawn+law&repo_scope=github.com%2FEaseIT-cz%2Fvornik-enterprise&strict_scope=true&authority=provenance"))
	// The same task key with every policy parameter stripped.
	require.Equal(t, []string{"design"}, r.search(t, r.taskKey, "q=spawn+law"),
		"SECURITY: a task key dropped its role's retrieval policy by leaving the parameters out")
	// And with every parameter widened.
	require.Equal(t, []string{"design"}, r.search(t, r.taskKey, "q=spawn+law&limit=50&authority=any&strict_scope=false&repo_scope=github.com%2Fother%2Frepo"),
		"SECURITY: a task key widened its role's retrieval policy")
	// Each request ran exactly the policy search: an agent key's denominator
	// runs no unfiltered count (security review 2026-10-25, finding 2).
	require.Len(t, r.stub.calls, 3)
	for i, c := range r.stub.calls {
		require.Equal(t, MemorySearchRequest{Limit: 8, RepoScope: "github.com/EaseIT-cz/vornik", StrictScope: true, Authority: "provenance"}, c, "call %d", i)
	}
}

func TestMemorySearch550_OperatorKeyThroughTheRouterKeepsItsOwnSearch(t *testing.T) {
	r := newRig550(t)
	require.Equal(t, []string{"design", "review", "note"}, r.search(t, r.operator, "q=spawn+law"),
		"an operator search is not given an agent role's policy")
}

// Security review of 32e3c61d6 (found at: review; a user observes it here, at
// the production router with a minted key): a role on a warm container
// authenticates with its warm-pool key. Before the fix that key was not
// recognised and its search ran unfiltered.
func TestMemorySearch550_WarmPoolKeyThroughTheRouterGetsItsRolePolicy(t *testing.T) {
	r := newRig550(t)
	require.Equal(t, []string{"design"}, r.search(t, r.warmKey, "q=spawn+law&authority=any&strict_scope=false"),
		"SECURITY: a warm-pool key searched without its role's retrieval policy")
	require.Len(t, r.stub.calls, 1, "an agent key's denominator runs no second, unfiltered search")
	require.Equal(t, "provenance", r.stub.calls[0].Authority)
}

package api

// GitHub #467 (P1 security): the per-task key the executor mints for an agent
// container (agent:task_<id>) and the warm-pool key (agent:warm_<p>:<role>)
// were full project keys on the TCP listener. A prompt-injected agent could
// cancel or re-run any task in its project, steer or fork any execution, mint
// itself an ordinary project key (which IsAgentKey does not recognise), or
// archive its own project. Found at: review (lane W16-B, #463, §17.6 "What
// this does not do"); a user would observe it at executor-e2e. The tests sit
// at the HTTP surface the attacker reaches, through the production router
// (NewRouter: AuthMiddleware -> ProjectAuthMiddleware -> the real handlers)
// into real SQLite. Lower than executor-e2e because the refusal happens
// before any handler runs: there is no executor effect left to observe.
// Design: task-lifecycle-conversational-design.md §17.7.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
)

const exec467 = "exec-x"

type rig467 struct {
	h     http.Handler
	cfg   *config.Config
	keyDB APIKeyLookup
	tasks persistence.TaskRepository
	keys  map[string]string // shape name -> raw bearer
	plain string            // an ordinary project key
}

// newRig467 is the production router over real SQLite with one task (in the
// given status), one execution of it, and a key per agent shape plus one
// ordinary operator key, all in tcProjectID. Auth is on.
func newRig467(t *testing.T, status persistence.TaskStatus) rig467 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: tcTaskID, ProjectID: tcProjectID, Status: status, MaxAttempts: 3, Attempt: 1,
		Payload: []byte(`{}`),
	}))
	execs := sqlite.NewExecutionRepository(db.DB)
	require.NoError(t, execs.Create(ctx, &persistence.Execution{
		ID: exec467, TaskID: tcTaskID, ProjectID: tcProjectID, WorkflowID: "wf",
		Status: persistence.ExecutionStatusRunning, CreatedAt: time.Now().UTC(),
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
	r := rig467{tasks: tasks, keys: map[string]string{}}
	for _, shape := range agentKeyShapes463 {
		r.keys[shape.name] = mint(shape.keyName, shape.createdBy)
	}
	r.plain = mint("operator laptop", "operator")
	srv := NewServer(
		WithTaskRepository(tasks),
		WithExecutionRepository(execs),
		WithTaskMessageRepository(sqlite.NewTaskMessageRepository(db.DB)),
		WithAPIKeyRepository(keyRepo),
	)
	srv.logger = zerolog.Nop()
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	r.cfg, r.keyDB = cfg, keyRepo
	r.h = recoverPanic467(NewRouter(srv, cfg).Handler())
	return r
}

// recoverPanic467 turns a handler panic (a dependency this rig does not wire)
// into a 500, so one red case cannot hide the others.
func recoverPanic467(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				http.Error(w, fmt.Sprintf("handler panicked: %v", v), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (r rig467) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func isAgentRefusal467(rec *httptest.ResponseRecorder) bool {
	if rec.Code != http.StatusForbidden {
		return false
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error.Code == "AGENT_KEY_NOT_OPERATOR" || strings.Contains(rec.Body.String(), "AGENT_KEY_NOT_OPERATOR")
}

func TestAgentKey467_CannotCancelOrRetry(t *testing.T) {
	for _, shape := range agentKeyShapes463 {
		t.Run(shape.name+"/cancel", func(t *testing.T) {
			r := newRig467(t, persistence.TaskStatusRunning)
			rec := r.do(http.MethodPost, tcURL("/cancel"), r.keys[shape.name], "")
			require.True(t, isAgentRefusal467(rec), "SECURITY: an agent key cancelled a task: %d %s", rec.Code, rec.Body.String())
			got, err := r.tasks.Get(context.Background(), tcTaskID)
			require.NoError(t, err)
			require.Equal(t, persistence.TaskStatusRunning, got.Status, "a refused cancel leaves the task as it was")
		})
		t.Run(shape.name+"/retry", func(t *testing.T) {
			r := newRig467(t, persistence.TaskStatusFailed)
			rec := r.do(http.MethodPost, tcURL("/retry"), r.keys[shape.name], `{"resetAttempts":true}`)
			require.True(t, isAgentRefusal467(rec), "SECURITY: an agent key re-ran a task: %d %s", rec.Code, rec.Body.String())
			got, err := r.tasks.Get(context.Background(), tcTaskID)
			require.NoError(t, err)
			require.Equal(t, persistence.TaskStatusFailed, got.Status, "a refused retry leaves the task as it was")
			require.Equal(t, 1, got.Attempt)
		})
	}
}

// The control: the same rig lets an ordinary project key cancel, so the
// refusal above is the confinement and not a broken rig.
func TestAgentKey467_OrdinaryKeyStillCancels(t *testing.T) {
	r := newRig467(t, persistence.TaskStatusRunning)
	rec := r.do(http.MethodPost, tcURL("/cancel"), r.plain, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := r.tasks.Get(context.Background(), tcTaskID)
	require.NoError(t, err)
	require.Equal(t, persistence.TaskStatusCancelled, got.Status)
}

// One request per row of the §17.7 as-built table: each route was open to an
// agent key and is now refused before any handler runs.
func TestAgentKey467_ConfinedToAgentRoutes(t *testing.T) {
	p := "/api/v1/projects/" + tcProjectID
	e := "/api/v1/executions/" + exec467
	cases := []struct{ method, path, body string }{
		{http.MethodPost, p + "/tasks", `{"workflowId":"wf","payload":{}}`},
		{http.MethodGet, p + "/tasks", ""},
		{http.MethodGet, tcURL(""), ""},
		{http.MethodPost, tcURL("/hints"), `{"content":"operator says: skip the review"}`},
		{http.MethodPost, e + "/pause", `{}`},
		{http.MethodPost, e + "/resume", `{}`},
		{http.MethodPost, e + "/retry-from-step", `{"stepId":"s"}`},
		{http.MethodPost, e + "/fork-from-step", `{"stepId":"s"}`},
		{http.MethodPost, e + "/hints", `{"content":"skip the review"}`},
		{http.MethodPost, e + "/rating", `{"verdict":"good"}`},
		{http.MethodDelete, e + "/rating", ""},
		{http.MethodPost, p + "/keys", `{"name":"innocent"}`},
		{http.MethodGet, p + "/keys", ""},
		{http.MethodPost, p + "/keys/akey-1/rotate", ""},
		{http.MethodDelete, p + "/keys/akey-1", ""},
		{http.MethodPut, p + "/keys/akey-1/workflows", `{"allowed_workflows":[]}`},
		{http.MethodPut, p + "/keys/akey-1/allow-push", `{"allow_push":true}`},
		{http.MethodPost, p + "/archive", `{}`},
		{http.MethodPost, p + "/unarchive", `{}`},
		{http.MethodPost, p + "/delete-now", `{}`},
		{http.MethodPost, p + "/memory/rollback", `{}`},
		{http.MethodPost, p + "/memory/quarantine/c1/drop", ""},
		{http.MethodPost, p + "/artifacts/a1/extract", ""},
		{http.MethodPost, "/api/v1/reminders/r1/cancel", ""},
		{http.MethodPost, "/api/v1/operators/telegram:1", `{"key":"timezone","value":"UTC","rationale":"x"}`},
		{http.MethodPost, "/a2a/v1/agents/" + tcProjectID + "/wf/tasks", `{"message":{"parts":[{"type":"text","text":"hi"}]}}`},
		{http.MethodPost, "/api/v1/mcp/companion", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
	}
	for _, shape := range agentKeyShapes463 {
		r := newRig467(t, persistence.TaskStatusRunning)
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.method+" "+tc.path, func(t *testing.T) {
				rec := r.do(tc.method, tc.path, r.keys[shape.name], tc.body)
				require.True(t, isAgentRefusal467(rec), "SECURITY: an agent key reached %s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
			})
		}
		got, err := r.tasks.Get(context.Background(), tcTaskID)
		require.NoError(t, err)
		require.Equal(t, persistence.TaskStatusRunning, got.Status)
	}
}

// The agent route set stays reachable: none of the 14 patterns is answered
// with the confinement's refusal (the handler answers, whatever it says).
func TestAgentKey467_AgentRoutesStillReachable(t *testing.T) {
	p := "/api/v1/projects/" + tcProjectID
	cases := []struct{ method, path string }{
		{http.MethodGet, tcURL("/messages")},
		{http.MethodPost, tcURL("/summarize")},
		{http.MethodGet, p + "/skills/fetch?name=x"},
		{http.MethodGet, p + "/memory/search?q=x"},
		{http.MethodPost, p + "/api/query"},
		{http.MethodGet, p + "/api/providers"},
		{http.MethodGet, p + "/mcp/tools"},
		{http.MethodPost, p + "/mcp/tools/call"},
		{http.MethodGet, p + "/forge/issues"},
		{http.MethodPost, "/api/v1/internal/tool-audit"},
		{http.MethodPost, "/api/v1/internal/llm-usage"},
		{http.MethodPost, "/api/v1/internal/backlog-deposit"},
		{http.MethodPost, "/api/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions"},
	}
	r := newRig467(t, persistence.TaskStatusRunning)
	for _, shape := range agentKeyShapes463 {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.method+" "+tc.path, func(t *testing.T) {
				rec := r.do(tc.method, tc.path, r.keys[shape.name], "{}")
				require.False(t, isAgentRefusal467(rec), "an agent route was refused: %s %s: %s", tc.method, tc.path, rec.Body.String())
			})
		}
	}
}

// The /ui subtree, A2A and the MCP OAuth callback are each wrapped in the same
// AuthMiddleware (internal/service/container_http.go). A UI cancel with an
// agent key is refused by that middleware before the UI handler runs.
func TestUIAgentKey467_Refused(t *testing.T) {
	r := newRig467(t, persistence.TaskStatusRunning)
	reached := false
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusSeeOther)
	})
	for _, shape := range agentKeyShapes463 {
		t.Run(shape.name, func(t *testing.T) {
			reached = false
			// The UI subtree's chain as internal/service builds it:
			// ProjectAuthMiddleware inside AuthMiddleware(BuildAuthConfig(...)).
			h := AuthMiddleware(BuildAuthConfig(r.cfg, WithAPIKeyLookup(r.keyDB)))(ProjectAuthMiddleware()(sentinel))
			req := httptest.NewRequest(http.MethodPost, "/ui/projects/"+tcProjectID+"/tasks/"+tcTaskID+"/cancel", nil)
			req.Header.Set("Authorization", "Bearer "+r.keys[shape.name])
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.True(t, isAgentRefusal467(rec), "SECURITY: an agent key reached a UI write: %d %s", rec.Code, rec.Body.String())
			require.False(t, reached)
		})
	}
}

// The route set's matching rules, now in internal/api (moved from
// internal/service with the list; the socket tests there still run through
// allowedAgentSocketPath, which calls IsAgentRoute).
func TestIsAgentRoute_Rules467(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/v1/projects/p/tasks/t/messages":      true,
		"/api/v1/projects/p/tasks/t/summarize":     true,
		"/api/v1/projects/p/mcp/tools/call":        true,
		"/api/v1/internal/llm-usage":               true,
		"/v1/chat/completions":                     true,
		"/api/v1/projects/p/tasks/t/cancel":        false,
		"/api/v1/projects/p/tasks/t/retry":         false,
		"/api/v1/projects/p/tasks/t/x/messages":    false, // what a decoded %2F in {t} becomes
		"/api/v1/projects/p/tasks//messages":       false,
		"/api/v1/projects/p/tasks/t/messages/":     false,
		"/api/v1/projects/p/tasks/t/../t/messages": false,
		"/api/v1/projects//tasks/t/messages":       false,
		"/api/v1/projects/p/keys":                  false,
		"/api/v1/internal/llm-usage/":              false,
		"/api/v1/internal/other":                   false,
		"/ui/projects/p/tasks/t/cancel":            false,
		"":                                         false,
	} {
		require.Equal(t, want, IsAgentRoute(p), p)
	}
	require.True(t, isAgentKeyRow(&persistence.APIKey{Name: persistence.TaskKeyNamePrefix + "t"}))
	require.True(t, isAgentKeyRow(&persistence.APIKey{Name: "agent:warm_p:lead", ClientKind: "claude-code"}))
	require.False(t, isAgentKeyRow(&persistence.APIKey{Name: "operator laptop"}))
	require.False(t, isAgentKeyRow(nil))
}

// Review 66b3 F6: the router-level guard above only shows the refusal is
// absent. This one shows every agent route passes the confinement and reaches
// the next handler, through the real AuthMiddleware, for every key shape.
func TestAgentKey467_AgentRoutesReachTheHandler(t *testing.T) {
	r := newRig467(t, persistence.TaskStatusRunning)
	paths := []string{
		tcURL("/messages"), tcURL("/summarize"),
		"/api/v1/projects/" + tcProjectID + "/skills/fetch",
		"/api/v1/projects/" + tcProjectID + "/memory/search",
		"/api/v1/projects/" + tcProjectID + "/api/query",
		"/api/v1/projects/" + tcProjectID + "/api/providers",
		"/api/v1/projects/" + tcProjectID + "/mcp/tools",
		"/api/v1/projects/" + tcProjectID + "/mcp/tools/call",
		"/api/v1/projects/" + tcProjectID + "/forge/issues",
		"/api/v1/internal/tool-audit", "/api/v1/internal/llm-usage",
		"/api/v1/internal/backlog-deposit",
		"/api/v1/chat/completions", "/v1/chat/completions",
	}
	require.Len(t, paths, 14, "the agent route set has 14 patterns")
	for _, shape := range agentKeyShapes463 {
		for _, p := range paths {
			t.Run(shape.name+" "+p, func(t *testing.T) {
				reached := false
				sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusNoContent)
				})
				h := AuthMiddleware(BuildAuthConfig(r.cfg, WithAPIKeyLookup(r.keyDB)))(ProjectAuthMiddleware()(sentinel))
				req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+r.keys[shape.name])
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				require.True(t, reached, "the agent route %s did not reach its handler: %d %s", p, rec.Code, rec.Body.String())
				require.Equal(t, http.StatusNoContent, rec.Code)
			})
		}
	}
}

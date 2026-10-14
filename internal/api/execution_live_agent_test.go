package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

const liveOperatorKey = "operator-static-key-for-live-tests"

// liveAgentHarness is a real router with real AuthMiddleware: the companion
// confinement, the handler's scoping and the projection are all on the path.
type liveAgentHarness struct {
	hs      *httptest.Server
	srv     *Server
	keyRepo *memAPIKeyRepo
	pub     livepubsub.Publisher
	execs   map[string]*persistence.Execution
	mu      sync.Mutex // guards execs: the terminal watcher reads them concurrently
	// onGet, when set, runs on every execution read (under mu): a seam that
	// tells a test the stream's watcher has ticked.
	onGet func(id string)
}

func newLiveAgentHarness(t *testing.T, opts ...ServerOption) *liveAgentHarness {
	t.Helper()
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"alpha":       {ID: "alpha"},
		"beta":        {ID: "beta"},
		"hermes--fin": {ID: "hermes--fin", Broker: true},
	})
	h := &liveAgentHarness{keyRepo: &memAPIKeyRepo{}, pub: livepubsub.New(200), execs: map[string]*persistence.Execution{}}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.onGet != nil {
				h.onGet(id)
			}
			if e, ok := h.execs[id]; ok {
				cp := *e
				return &cp, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	taskRepo := &mocks.MockTaskRepository{GetFunc: func(context.Context, string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}}
	h.srv = NewServer(append([]ServerOption{
		WithLogger(zerolog.Nop()),
		WithLiveSubscriber(h.pub),
		WithExecutionRepository(execRepo),
		WithTaskRepository(taskRepo),
		WithMetricsRegistry(prometheus.NewRegistry()),
	}, opts...)...)
	h.srv.apiKeyRepo = h.keyRepo
	h.srv.projectRegistry = reg
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{liveOperatorKey}
	h.hs = httptest.NewServer(NewRouter(h.srv, cfg).Handler())
	t.Cleanup(h.hs.Close)
	return h
}

func (h *liveAgentHarness) addExec(id, project string, status persistence.ExecutionStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.execs[id] = &persistence.Execution{ID: id, TaskID: "task-" + id, ProjectID: project, WorkflowID: "wf", Status: status}
}

func (h *liveAgentHarness) url(id, query string) string {
	return "ws" + strings.TrimPrefix(h.hs.URL, "http") + "/api/v1/executions/" + id + "/live" + query
}

func (h *liveAgentHarness) dial(t *testing.T, id, bearer string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	hdr := http.Header{"Origin": []string{h.hs.URL}}
	if bearer != "" {
		hdr.Set("Authorization", "Bearer "+bearer)
	}
	return websocket.Dial(ctx, h.url(id, ""), &websocket.DialOptions{HTTPHeader: hdr})
}

func (h *liveAgentHarness) plainGet(t *testing.T, id, bearer, query string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.hs.URL+"/api/v1/executions/"+id+"/live"+query, nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, body, err := conn.Read(ctx)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

func sendHello(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"last_seq":0}`)))
}

// GitHub #92 (2026-10-08): a companion key reaches the live route of its own
// project's execution through the real middleware, and frames are projected
// (a planted secret in a tool error arrives withheld).
func TestExecutionLive_CompanionKeyOwnProjectStreams(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "w"})
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindToolCallFinished, livepubsub.ToolCallFinishedPayload{CallID: "c", Err: "bad " + plantedToken})

	conn, _, err := h.dial(t, "exec-a", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	first := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindStepStarted, first["kind"])
	second := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindWithheld, second["kind"])
	b, _ := json.Marshal(second)
	assert.NotContains(t, string(b), "ghp_")
}

// GitHub #92 (2026-10-08): another namespace's execution answers exactly as
// an unknown one, status and body byte for byte (no existence leak).
func TestExecutionLive_CompanionKeyOtherNamespaceIs404(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-b", "beta", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	codeForeign, bodyForeign := h.plainGet(t, "exec-b", raw, "")
	codeUnknown, bodyUnknown := h.plainGet(t, "exec-zz", raw, "")
	assert.Equal(t, http.StatusNotFound, codeForeign)
	assert.Equal(t, codeUnknown, codeForeign)
	assert.Equal(t, bodyUnknown, bodyForeign)
}

// GitHub #92 (2026-10-08): an agent-admin key covers its namespace's projects
// and reads them through the broker view (every agent project is broker).
func TestExecutionLive_AgentAdminKeyCoversNamespaceProject(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-n", "hermes--fin", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "hermes--home", nil)
	h.keyRepo.rows[len(h.keyRepo.rows)-1].AgentAdmin = true
	h.keyRepo.rows[len(h.keyRepo.rows)-1].AgentNamespace = "hermes"
	h.pub.Publish(context.Background(), "exec-n", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "r", Model: "secret-model"})

	conn, _, err := h.dial(t, "exec-n", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	frame := readFrame(t, conn)
	payload := frame["payload"].(map[string]any)
	assert.Equal(t, map[string]any{"step_id": "s1", "role": "r"}, payload, "broker view: lifecycle fields only")
}

// GitHub #92 (2026-10-08): a companion secret in ?key= authenticates nothing:
// 401, no upgrade (a key in a URL ends up in access logs).
func TestExecutionLive_CompanionKeyQueryParamRefused(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	code, _ := h.plainGet(t, "exec-a", "", "?key="+raw)
	assert.Equal(t, http.StatusUnauthorized, code)
}

// GitHub #92 (2026-10-08): the operator path is unchanged: a static operator
// key gets today's bytes (no projection).
func TestExecutionLive_OperatorPathUnchanged(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindToolCallFinished, livepubsub.ToolCallFinishedPayload{CallID: "c", Err: "raw tool error"})
	conn, _, err := h.dial(t, "exec-a", liveOperatorKey)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	frame := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindToolCallFinished, frame["kind"])
	assert.Equal(t, "raw tool error", frame["payload"].(map[string]any)["err"])
}

// GitHub #92 (2026-10-08): a key holds at most two live streams; the third is
// refused before the upgrade.
func TestExecutionLive_AgentStreamCapPerKey(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	for i := 0; i < liveAgentStreamsPerKey; i++ {
		conn, _, err := h.dial(t, "exec-a", raw)
		require.NoError(t, err)
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		sendHello(t, conn)
	}
	code, _ := h.plainGet(t, "exec-a", raw, "")
	assert.Equal(t, http.StatusTooManyRequests, code)
}

// GitHub #92 (2026-10-08), Change C1: the companion allowlist gains exactly
// one path shape; nothing near it opens.
func TestIsCompanionLivePath_ExactShapeOnly(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/v1/executions/exec_1/live":          true,
		"/api/v1/executions/exec_1/live/":         false,
		"/api/v1/executions//live":                false,
		"/api/v1/executions/a/b/live":             false,
		"/api/v1/executions/exec_1/pause":         false,
		"/api/v1/executions/exec_1/live/x":        false,
		"/api/v1/executions/exec_1":               false,
		"/api/v1/projects/p/tasks/t/logs":         false,
		"/api/v1/executions/exec_1/livez":         false,
		"/api/v1/executions/exec_1/hints/../live": false,
	} {
		assert.Equal(t, want, isCompanionAllowedPath(p), p)
	}
}

// GitHub #92 (2026-10-08), A0.4: every stream ends at terminal status with a
// final {"kind":"closed"} frame, which the original design promised and
// nothing sent.
func TestExecutionLive_TerminalExecutionSendsClosed(t *testing.T) {
	h := newLiveAgentHarness(t, fastTerminalCheck)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusCompleted)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	conn, _, err := h.dial(t, "exec-a", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	frame := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindClosed, frame["kind"])
}

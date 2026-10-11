package api

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/observability/logring"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/registry"
)

type diagFixture struct {
	srv      *Server
	keyRepo  *memAPIKeyRepo
	adminRaw string
	plainRaw string
	ring     *logring.Ring
	tasks    map[string]*persistence.Task
}

// newDiagFixture: namespace "hermes" with projects hermes--home and
// hermes--fin (broker), and another namespace's project other--x.
func newDiagFixture(t *testing.T) *diagFixture {
	t.Helper()
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"hermes--home": {ID: "hermes--home", Broker: true},
		"hermes--fin":  {ID: "hermes--fin", Broker: true},
		"other--x":     {ID: "other--x", Broker: true},
		"alpha":        {ID: "alpha"},
	})
	f := &diagFixture{keyRepo: &memAPIKeyRepo{}, ring: logring.New(100, time.Hour), tasks: map[string]*persistence.Task{}}
	taskRepo := &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		if tk, ok := f.tasks[id]; ok {
			cp := *tk
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}}
	f.srv = &Server{logger: zerolog.Nop(), apiKeyRepo: f.keyRepo, taskRepo: taskRepo, projectRegistry: reg,
		daemonLogRing: f.ring}
	st := newLiveLogState()
	f.srv.liveLogsState = st
	f.adminRaw, _ = seedCompanionKey(t, f.keyRepo, "hermes--home", nil)
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentAdmin = true
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentNamespace = "hermes"
	f.plainRaw, _ = seedCompanionKey(t, f.keyRepo, "alpha", nil)
	return f
}

func (f *diagFixture) logLine(fields string) {
	f.ring.Append([]byte(`{"level":"error",` + fields + "}\n"))
}

func (f *diagFixture) call(t *testing.T, raw, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	text, isErr := callTool(t, f.srv, raw, tool, args)
	if isErr {
		return nil, text, true
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out, text, false
}

func diagLines(out map[string]any) []map[string]any {
	raw, _ := out["lines"].([]any)
	res := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		res = append(res, r.(map[string]any))
	}
	return res
}

// GitHub #92 (2026-10-08): an agent-admin key reads only its namespace's
// projects' lines, including a task-only line resolved to its project at read.
func TestDaemonLog_AgentAdminSeesOnlyNamespaceProjects(t *testing.T) {
	f := newDiagFixture(t)
	f.tasks["t-fin"] = &persistence.Task{ID: "t-fin", ProjectID: "hermes--fin"}
	f.tasks["t-other"] = &persistence.Task{ID: "t-other", ProjectID: "other--x"}
	f.logLine(`"component":"scheduler","project_id":"hermes--fin","message":"mine"`)
	f.logLine(`"component":"scheduler","project_id":"other--x","message":"theirs"`)
	f.logLine(`"task_id":"t-fin","message":"mine by task"`)
	f.logLine(`"task_id":"t-other","message":"theirs by task"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "theirs")
	lines := diagLines(out)
	require.Len(t, lines, 2)
	assert.Equal(t, "hermes--fin", lines[0]["project"])
	assert.Equal(t, "t-fin", lines[1]["task_id"])
	assert.Equal(t, "hermes--fin", lines[1]["project"])

	_, text, isErr = f.call(t, f.adminRaw, "daemon_log", map[string]any{"project": "other--x"})
	require.True(t, isErr)
	assert.Contains(t, text, "not found")
	_, text2, isErr2 := f.call(t, f.adminRaw, "daemon_log", map[string]any{"task_id": "t-other"})
	require.True(t, isErr2)
	_, unknown, _ := f.call(t, f.adminRaw, "daemon_log", map[string]any{"task_id": "t-nope"})
	assert.Equal(t, replaceID(unknown, "t-nope", "t-other"), text2, "another namespace's task is an unknown one")
}

func replaceID(s, from, to string) string {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+len(from) <= len(b) && string(b[i:i+len(from)]) == from {
			out = append(out, to...)
			i += len(from)
			continue
		}
		out = append(out, b[i])
		i++
	}
	return string(out)
}

// GitHub #92 (2026-10-08): daemon_log and doctor are agent-admin tools: a plain
// companion key is neither offered them nor able to call them.
func TestDaemonLog_PlainCompanionKeyRefused(t *testing.T) {
	f := newDiagFixture(t)
	key, err := f.keyRepo.LookupActiveByHash(context.Background(), apikey.Hash(f.plainRaw))
	require.NoError(t, err)
	for _, d := range f.srv.companionToolsFor(key) {
		assert.NotEqual(t, "daemon_log", d.Name)
		assert.NotEqual(t, "doctor", d.Name)
	}
	for _, tool := range []string{"daemon_log", "doctor"} {
		_, text, isErr := f.call(t, f.plainRaw, tool, map[string]any{})
		assert.True(t, isErr, tool)
		assert.Contains(t, text, "AGENT_ADMIN_ONLY", tool)
	}
}

// GitHub #92 (2026-10-08): "see why an MCP server is not connected". A
// connect failure refused with 401 shows the closed code auth_rejected and
// never the upstream text (the ring never stored it).
func TestDaemonLog_MCPConnectFailureShowsClosedCode(t *testing.T) {
	f := newDiagFixture(t)
	registry.SeedForTest(f.srv.projectRegistry, map[string]*registry.Project{
		"hermes--fin": {ID: "hermes--fin", Broker: true, MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "mail"}}}},
	})
	f.logLine(`"component":"mcp","project":"hermes--fin","server":"mail","http_status":401,"error":"401 bad token sk-ant-abcdefghijklmnop","message":"mcp: failed to connect"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{"project": "hermes--fin"})
	require.False(t, isErr, text)
	lines := diagLines(out)
	require.Len(t, lines, 1)
	assert.Equal(t, "auth_rejected", lines[0]["code"])
	assert.Equal(t, "mail", lines[0]["server"])
	assert.NotContains(t, text, "sk-ant")
	assert.NotContains(t, text, "bad token")
}

// GitHub #92 (2026-10-08): a line whose message holds a secret is withheld whole.
func TestDaemonLog_FreeTextLineOmitted(t *testing.T) {
	f := newDiagFixture(t)
	f.logLine(`"component":"x","project":"hermes--fin","message":"using ` + plantedToken + `"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "ghp_")
	lines := diagLines(out)
	require.Len(t, lines, 1)
	assert.Equal(t, true, lines[0]["message_omitted"], "not an allowlisted literal: omitted, so the token never leaves")
	_, hasMsg := lines[0]["message"]
	assert.False(t, hasMsg)
}

// GitHub #92 (2026-10-08): task_explain answers another namespace's task as
// an unknown one.
func TestTaskExplain_OtherNamespaceNotFound(t *testing.T) {
	f := newDiagFixture(t)
	f.tasks["t-other"] = &persistence.Task{ID: "t-other", ProjectID: "other--x"}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	_, text, isErr := f.call(t, f.adminRaw, "task_explain", map[string]any{"task_id": "t-other"})
	require.True(t, isErr)
	assert.Contains(t, text, "not found")
}

// failingTailFetcher fails the test if the explain path asks for container
// logs (podman logs: a process spawn from an API path).
type failingTailFetcher struct{ t *testing.T }

func (f failingTailFetcher) TaskLogs(context.Context, string, int) (string, error) {
	f.t.Error("task_explain reached the container log fetcher (podman logs)")
	return "", nil
}

// GitHub #92 (2026-10-08): for a broker task (any agent-admin read) explain
// returns only the closed class and the playbook id; the raw error and the
// summary never leave.
func TestTaskExplain_BrokerTaskReturnsClassAndPlaybookOnly(t *testing.T) {
	f := newDiagFixture(t)
	cls := persistence.TaskFailureClassTimeout
	f.tasks["t-fin"] = &persistence.Task{ID: "t-fin", ProjectID: "hermes--fin", Status: persistence.TaskStatusFailed,
		LastError: strp("timed out reading " + brokerSentinel), LastErrorClass: &cls}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	out, text, isErr := f.call(t, f.adminRaw, "task_explain", map[string]any{"task_id": "t-fin"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, brokerSentinel)
	keys := []string{}
	for k := range out {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"task_id", "status", "error_class", "playbook_id"}, keys)
	assert.Equal(t, brokerErrTimeout, out["error_class"])
	assert.Equal(t, persistence.TaskFailureClassTimeout, out["playbook_id"])
}

// GitHub #92 (2026-10-08): the companion view of explain is the summary and
// inputs, scrubbed; no container log is fetched.
func TestTaskExplain_CompanionViewScrubbed(t *testing.T) {
	f := newDiagFixture(t)
	cls := persistence.TaskFailureClassToolError
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusFailed,
		LastError: strp("failed with " + plantedToken + " at /home/alice/x"), LastErrorClass: &cls}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	out, text, isErr := f.call(t, f.plainRaw, "task_explain", map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "ghp_")
	assert.NotContains(t, text, "alice")
	assert.NotEmpty(t, out["playbook"])
}

// GitHub #92 (2026-10-08): the agent-admin doctor reports only its
// namespace's projects: a pending server elsewhere does not appear, and the
// state is closed codes and counts only.
func TestDoctor_AgentAdminChecksFilteredToNamespace(t *testing.T) {
	f := newDiagFixture(t)
	since := time.Now().Add(-time.Hour)
	f.srv.diagMCPPending = func() (int, []mcp.PendingServer) {
		return 3, []mcp.PendingServer{
			{ProjectID: "hermes--fin", Server: "mail", Since: since, Attempts: 4, LastError: "401 bad token sk-ant-xyz", HTTPStatus: 401, Cause: &mcp.CallError{Status: 401}},
			{ProjectID: "other--x", Server: "theirs", Since: since, Attempts: 1, LastError: "nope"},
		}
	}
	f.srv.diagMCPNoGrant = func() []mcpconnect.NoGrant {
		return []mcpconnect.NoGrant{{ProjectID: "hermes--home", Server: "cal"}, {ProjectID: "other--x", Server: "x"}}
	}
	out, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "other--x")
	assert.NotContains(t, text, "theirs")
	assert.NotContains(t, text, "sk-ant")
	assert.NotContains(t, text, "bad token")
	servers, _ := out["mcp_servers"].([]any)
	require.Len(t, servers, 2, text)
	first := servers[0].(map[string]any)
	assert.Equal(t, "hermes--fin", first["project"])
	assert.Equal(t, "pending", first["state"])
	assert.Equal(t, "auth_rejected", first["code"])
	second := servers[1].(map[string]any)
	assert.Equal(t, "withheld", second["state"])
	assert.Equal(t, "credential_missing", second["code"])
}

// GitHub #92 (2026-10-08), Change C8: no process spawn from the diagnosis
// tools' file.
func TestDiagTools_NoProcessSpawn(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "companion_diag_tools.go", nil, parser.ImportsOnly)
	require.NoError(t, err)
	for _, imp := range parsed.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		for _, bad := range []string{"os/exec", "vornik.io/vornik/internal/spawn",
			"vornik.io/vornik/internal/runtime", "vornik.io/vornik/internal/executor"} {
			assert.NotEqual(t, bad, path)
		}
	}
}

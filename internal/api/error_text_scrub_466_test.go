package api

// GitHub #466 (security, the rest of #440): a project-scoped key read the raw
// task last_error and execution error_message through GET task, the task
// list, GET execution and the execution list, read every live event in the
// operator view on the execution socket, and a companion key read the raw
// last_error through the status and result tools. #440 had scrubbed only the
// logs and explain routes, so a secret those routes withhold was still served
// a click away. Found at: review (the #440 lane's open decision 1); a user
// observes it on the HTTP route, the socket and the MCP tool, so these tests
// run the real router (AuthMiddleware with a DB project key, then the
// project gate) over real SQLite repositories, and plant secrets after a
// newline and as a JSON object key inside the error text
// (process-spawn-law design, "Amendment 2026-10-21 (#466)").

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/report"
)

const adminKey466 = "sk-admin-466"

// errorText466 carries the three planted shapes: a GitHub token after
// "Bearer", an AWS key at the start of a line (after a newline), and a Slack
// key used as a JSON object key, plus a clean line that must survive.
var errorText466 = "step build failed: Authorization: Bearer " + plantedToken440 +
	"\n" + awsKey440 +
	"\n{\"" + slackKey440 + "\":1}" +
	"\nupstream said: {\"error\":\"denied\\n" + escapedToken466 + "\"}" +
	"\nexit status 1"

// escapedToken466 follows a literal backslash-n (a quoted JSON error body),
// which the line scrubber's \b-anchored nets missed (design B3, D5).
var escapedToken466 = "ghp_" + strings.Repeat("E5cQ9pLs2W", 4)[:36]

type harness466 struct {
	hs           *httptest.Server
	srv          *Server
	tasks        *sqlite.TaskRepository
	pub          livepubsub.Publisher
	projectKey   string
	companionKey string
}

func newHarness466(t *testing.T) *harness466 {
	t.Helper()
	return newHarness466With(t, nil)
}

// newHarness466With is newHarness466 with extra static api.api_keys entries
// (not admin-class) and extra server options (GitHub #491).
func newHarness466With(t *testing.T, extraStaticKeys []string, opts ...ServerOption) *harness466 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	wf := "wf-466"
	now := time.Now().UTC()
	le := errorText466
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: "t466", ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), LastError: &le, CreatedAt: now, UpdatedAt: now,
	}))
	em := errorText466
	require.NoError(t, execs.Create(ctx, &persistence.Execution{
		ID: "e466", TaskID: "t466", ProjectID: "alpha", WorkflowID: wf,
		Status: persistence.ExecutionStatusRunning, ErrorMessage: &em,
	}))

	h := &harness466{pub: livepubsub.New(100), tasks: tasks}
	keyRepo := &memAPIKeyRepo{}
	pk, err := apikey.Generate("alpha")
	require.NoError(t, err)
	require.NoError(t, keyRepo.Create(ctx, &persistence.APIKey{
		ID: "akey-466", ProjectID: "alpha", Name: "ci", KeyHash: apikey.Hash(pk),
		KeyPrefix: apikey.DisplayPrefix(pk), CreatedAt: now,
	}))
	h.projectKey = pk
	h.companionKey, _ = seedCompanionKey(t, keyRepo, "alpha", nil)

	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"alpha": {ID: "alpha"}})
	h.srv = NewServer(append([]ServerOption{
		WithLogger(zerolog.Nop()),
		WithTaskRepository(tasks), WithExecutionRepository(execs),
		WithLiveSubscriber(h.pub),
		WithMetricsRegistry(prometheus.NewRegistry()),
		WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey466}}),
	}, opts...)...)
	h.srv.apiKeyRepo = keyRepo
	h.srv.projectRegistry = reg
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = append([]string{adminKey466}, extraStaticKeys...)
	h.hs = httptest.NewServer(NewRouter(h.srv, cfg).Handler())
	t.Cleanup(h.hs.Close)
	return h
}

func (h *harness466) get(t *testing.T, path, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.hs.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// decodedStrings466 is every string inside a JSON body, decoded, so an
// assertion is not fooled by the escaping of the raw bytes.
func decodedStrings466(t *testing.T, body string) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(body), &v), body)
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch tv := x.(type) {
		case string:
			b.WriteString(tv)
			b.WriteByte('\n')
		case map[string]any:
			for k, y := range tv {
				b.WriteString(k)
				b.WriteByte('\n')
				walk(y)
			}
		case []any:
			for _, y := range tv {
				walk(y)
			}
		}
	}
	walk(v)
	return b.String()
}

func assertNoSecret466(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range []string{plantedToken440, awsKey440, slackKey440, escapedToken466} {
		assert.NotContains(t, text, s, "%s served a planted secret to a project-scoped caller", where)
	}
}

var restPaths466 = []string{
	"/api/v1/projects/alpha/tasks/t466",
	"/api/v1/projects/alpha/tasks",
	"/api/v1/executions/e466",
	"/api/v1/projects/alpha/executions",
}

func TestTaskReads466_ProjectKeySeesNoSecret(t *testing.T) {
	h := newHarness466(t)
	for _, p := range restPaths466 {
		code, body := h.get(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assertNoSecret466(t, p, body)
		text := decodedStrings466(t, body)
		assertNoSecret466(t, p, text)
		assert.Contains(t, text, report.WithheldLine, p)
		assert.Contains(t, text, "exit status 1", "%s: a line with no secret is still served", p)

		// The admin-class caller keeps the operator view.
		code, body = h.get(t, p, adminKey466)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assert.Contains(t, decodedStrings466(t, body), plantedToken440, "%s: the operator view must stay unchanged", p)
	}
}

// Fail closed: with no scrubber every line of the field is withheld; the
// route itself still answers.
func TestTaskReads466_NoScrubberWithholdsEveryLine(t *testing.T) {
	h := newHarness466(t)
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}
	for _, p := range restPaths466 {
		code, body := h.get(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedStrings466(t, body)
		assertNoSecret466(t, p, text)
		assert.NotContains(t, text, "exit status 1", "%s: no scrubber serves no line", p)
	}
}

func (h *harness466) dial(t *testing.T, bearer string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	hdr := http.Header{"Origin": []string{h.hs.URL}, "Authorization": []string{"Bearer " + bearer}}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.hs.URL, "http")+"/api/v1/executions/e466/live", &websocket.DialOptions{HTTPHeader: hdr})
}

func (h *harness466) publishLive(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	h.pub.Publish(ctx, "e466", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "build", Role: "coder"})
	h.pub.Publish(ctx, "e466", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: "curl -H 'Authorization: Bearer " + plantedToken440 + "'"})
	h.pub.Publish(ctx, "e466", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c2", Err: "result:\n" + awsKey440})
	h.pub.Publish(ctx, "e466", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c3", OutputJSON: json.RawMessage(`{"result:\n` + slackKey440 + `":1}`)})
	// A token stream can split a secret across deltas, which no per-event
	// scrub sees: the scrubbed view drops llm_token.
	h.pub.Publish(ctx, "e466", livepubsub.KindLLMToken, map[string]any{"delta": plantedToken440[:10]})
	h.pub.Publish(ctx, "e466", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c4", Err: "plain failure"})
}

// readUntil466 reads frames until one whose payload has call_id c4 (the last
// event published), returning everything read.
func readUntil466(t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i := 0; i < 20; i++ {
		f := readFrame(t, conn)
		out = append(out, f)
		if p, ok := f["payload"].(map[string]any); ok && p["call_id"] == "c4" {
			return out
		}
	}
	t.Fatalf("the last event never arrived: %v", out)
	return nil
}

func TestExecutionLive466_ProjectKeyReadsScrubbedView(t *testing.T) {
	h := newHarness466(t)
	h.publishLive(t)

	conn, resp, err := h.dial(t, h.projectKey)
	require.NoError(t, err, "%v", resp)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	frames := readUntil466(t, conn)
	withheld := 0
	for _, f := range frames {
		b, _ := json.Marshal(f)
		assertNoSecret466(t, "the live socket", string(b))
		assertNoSecret466(t, "the live socket", decodedStrings466(t, string(b)))
		assert.NotEqual(t, livepubsub.KindLLMToken, f["kind"], "llm_token is dropped in the scrubbed view")
		if f["kind"] == livepubsub.KindWithheld {
			withheld++
		}
	}
	assert.Equal(t, 3, withheld, "each event holding a secret is withheld whole")
	assert.Equal(t, livepubsub.KindStepStarted, frames[0]["kind"], "lifecycle events pass")
	assert.Equal(t, "plain failure", frames[len(frames)-1]["payload"].(map[string]any)["err"], "clean content passes")

	// The admin-class caller keeps the operator bytes.
	conn2, _, err := h.dial(t, adminKey466)
	require.NoError(t, err)
	defer func() { _ = conn2.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn2)
	var all strings.Builder
	for _, f := range readUntil466(t, conn2) {
		b, _ := json.Marshal(f)
		all.Write(b)
	}
	assert.Contains(t, all.String(), plantedToken440, "the operator view must stay byte-identical")
}

func TestExecutionLive466_NoScrubberRefusesProjectKey(t *testing.T) {
	h := newHarness466(t)
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}
	h.publishLive(t)

	_, resp, err := h.dial(t, h.projectKey)
	require.Error(t, err, "the socket must not open with no scrubber")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func (h *harness466) companionTool(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req, err := http.NewRequest(http.MethodPost, h.hs.URL+"/api/v1/mcp/companion", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.companionKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	text, isErr := decodeToolText(t, decodeJSONRPC(t, b))
	require.False(t, isErr, text)
	return text
}

func TestCompanionStatusResult466_LastErrorScrubbed(t *testing.T) {
	h := newHarness466(t)
	for _, tool := range []string{"status", "result"} {
		text := h.companionTool(t, tool, map[string]any{"task_id": "t466"})
		assertNoSecret466(t, tool, text)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &out), text)
		le, _ := out["last_error"].(string)
		assertNoSecret466(t, tool, le)
		assert.Contains(t, le, report.WithheldLine, "%s: %s", tool, text)
		assert.Contains(t, le, "exit status 1", tool)
	}
}

// D7: companion task_explain rendered with a renderer copy whose Scrub was
// nil, so Render cut last_error at 1500 runes before companionExplainView
// scrubbed it, and a token straddling the cut left a fragment no net matches.
func TestCompanionExplain466_ScrubsBeforeTheCut(t *testing.T) {
	h := newHarness466(t)
	le := strings.Repeat("a", 1490) + " " + plantedToken440
	now := time.Now().UTC()
	wf := "wf-466"
	require.NoError(t, h.tasks.Create(context.Background(), &persistence.Task{
		ID: "t466cut", ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), LastError: &le, CreatedAt: now, UpdatedAt: now,
	}))
	h.srv.explainRenderer = postmortem.NewRenderer(h.tasks, nil, nil, nil, nil)

	text := h.companionTool(t, "task_explain", map[string]any{"task_id": "t466cut"})
	assert.NotContains(t, text, plantedToken440[:8], "a fragment of the token survived the cut")
	assert.Contains(t, text, report.WithheldLine)
}

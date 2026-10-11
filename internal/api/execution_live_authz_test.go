package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fastTerminalCheck shortens the stream re-check interval for one test.
func fastTerminalCheck(t *testing.T) {
	t.Helper()
	orig := liveTerminalCheckEvery
	liveTerminalCheckEvery = 50 * time.Millisecond
	t.Cleanup(func() { liveTerminalCheckEvery = orig })
}

// expectClosedNotFound reads until the server closes the socket and checks it
// closed as NOT_FOUND (policy violation), not by the client or a timeout.
func expectClosedNotFound(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		require.NoError(t, ctx.Err(), "the stream was not closed by the server")
		assert.Equal(t, websocket.StatusPolicyViolation, websocket.CloseStatus(err), err.Error())
		assert.Contains(t, err.Error(), "NOT_FOUND")
		return
	}
}

// review-20261008-f680 finding 1 (HIGH), GitHub #92 (2026-10-08): an agent
// socket was authorised only at entry. When the execution's project moves out
// of the key's coverage mid-stream, the stream closes as NOT_FOUND.
func TestExecutionLive_AgentStreamClosesWhenProjectReassigned(t *testing.T) {
	fastTerminalCheck(t)
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	conn, _, err := h.dial(t, "exec-a", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)

	h.mu.Lock()
	h.execs["exec-a"].ProjectID = "beta"
	h.mu.Unlock()
	expectClosedNotFound(t, conn)
}

// review-20261008-f680 finding 1 (HIGH), GitHub #92 (2026-10-08): a key
// revoked mid-stream loses the stream on the next re-check.
func TestExecutionLive_AgentStreamClosesWhenKeyRevoked(t *testing.T) {
	fastTerminalCheck(t)
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	conn, _, err := h.dial(t, "exec-a", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)

	registerLiveLogsMetrics()
	before := testutil.ToFloat64(liveAgentStreamClosed.WithLabelValues("revoked"))
	h.keyRepo.mu.Lock()
	now := time.Now()
	h.keyRepo.rows[len(h.keyRepo.rows)-1].RevokedAt = &now // memAPIKeyRepo.LookupActiveByHash skips revoked rows, as both drivers do
	h.keyRepo.mu.Unlock()
	expectClosedNotFound(t, conn)
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(liveAgentStreamClosed.WithLabelValues("revoked")) == before+1
	}, 2*time.Second, 10*time.Millisecond, "the close is counted with reason revoked (review-20261008-2892 finding 2)")
}

// review-20261008-f680 finding 3 (MEDIUM), GitHub #92 (2026-10-08): a
// cross-project call. The parent project's key cannot open the child
// execution's socket (another project), and on its own stream the
// cross_project_call_started names of the child project arrive blanked,
// through the real router.
func TestExecutionLive_CrossProjectChildIsolated(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-parent", "alpha", persistence.ExecutionStatusRunning)
	h.addExec("exec-child", "beta", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)

	code, _ := h.plainGet(t, "exec-child", raw, "")
	assert.Equal(t, http.StatusNotFound, code, "the child's execution is another project's")

	h.pub.Publish(context.Background(), "exec-parent", livepubsub.KindCrossProjectCallStarted,
		livepubsub.CrossProjectCallStartedPayload{CPCId: "cpc-1", CalleeProject: "beta", CalleeWorkflow: "wf-beta",
			CalleeTaskID: "task-exec-child", StepID: "handoff"})
	conn, _, err := h.dial(t, "exec-parent", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	frame := readFrame(t, conn)
	require.Equal(t, livepubsub.KindCrossProjectCallStarted, frame["kind"])
	p := frame["payload"].(map[string]any)
	assert.Equal(t, "", p["callee_project"])
	assert.Equal(t, "", p["callee_workflow"])
	assert.Equal(t, "", p["callee_task_id"])
	assert.Equal(t, "cpc-1", p["cpc_id"])
	assert.Equal(t, "handoff", p["step_id"], "the caller's own step id stays")
}

// review-20261008-f680 finding 2 (MEDIUM), GitHub #92 (2026-10-08): the
// agent-admin read contract. An agent-admin key always reads the broker view,
// even for a namespace project that is not (or not yet) a loaded broker
// project, so it never sees step output or a foreign project's names.
func TestLiveContract_AgentAdminKeyAlwaysBrokerView(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"hermes--plain": {ID: "hermes--plain"}})
	srv := &Server{projectRegistry: reg}
	wf := "wf-plain"
	key := &persistence.APIKey{ProjectID: "hermes--home", ClientKind: "hermes", AgentAdmin: true, AgentNamespace: "hermes"}
	task := &persistence.Task{ID: "t", ProjectID: "hermes--plain", WorkflowID: &wf}
	assert.True(t, srv.brokerViewFor(key, task))
	assert.Equal(t, liveViewBroker, srv.liveViewFor(key, task))

	evt := livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallStarted,
		Payload: livepubsub.CrossProjectCallStartedPayload{CPCId: "c", CalleeProject: "hermes--plain"}}
	_, keep := projectLiveEvent(evt, srv.liveViewFor(key, task), nil, func(string) bool { return true })
	assert.False(t, keep, "the broker view drops cross-project kinds, covered or not")
}

// review-20261008-f680 finding 2 (MEDIUM), GitHub #92 (2026-10-08): the other
// direction. A plain companion key on its own non-broker project reads the
// companion view: its own project's names stay, a foreign project's go.
func TestLiveContract_PlainCompanionKeySeesOwnNamesOnly(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"alpha": {ID: "alpha"}})
	srv := &Server{projectRegistry: reg}
	key := &persistence.APIKey{ProjectID: "alpha", ClientKind: "claude-code"}
	task := &persistence.Task{ID: "t", ProjectID: "alpha"}
	view := srv.liveViewFor(key, task)
	require.Equal(t, liveViewCompanion, view)
	scrub := testScrubber(t)
	covers := func(p string) bool { return keyCoversProject(key, p) }
	own, ok := projectLiveEvent(livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallReceived,
		Payload: livepubsub.CrossProjectCallReceivedPayload{CPCId: "c", CallerProject: "alpha", CallerTaskID: "t0"}}, view, scrub, covers)
	require.True(t, ok)
	assert.Equal(t, "alpha", payloadMap(t, own)["caller_project"])
	foreign, ok := projectLiveEvent(livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallReceived,
		Payload: livepubsub.CrossProjectCallReceivedPayload{CPCId: "c", CallerProject: "beta", CallerTaskID: "t0"}}, view, scrub, covers)
	require.True(t, ok)
	assert.Equal(t, "", payloadMap(t, foreign)["caller_project"])
	assert.Equal(t, "", payloadMap(t, foreign)["caller_task_id"])
}

// review-20261008-f680 finding 7 (LOW), GitHub #92 (2026-10-08): agent keys
// are not browsers. Their upgrade is same-origin only even when the operator
// configured extra UI origins, and an agent request carrying a browser
// session cookie is refused.
func TestExecutionLive_AgentOriginPolicy(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.srv.liveAllowedOrigins = []string{"ui.example.com"}
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)

	dial := func(hdr http.Header) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hdr.Set("Authorization", "Bearer "+raw)
		c, _, err := websocket.Dial(ctx, h.url("exec-a", ""), &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
		return err
	}
	assert.Error(t, dial(http.Header{"Origin": []string{"https://ui.example.com"}}),
		"a configured operator UI origin does not admit an agent key")
	assert.Error(t, dial(http.Header{"Origin": []string{"https://evil.example"}}))
	assert.Error(t, dial(http.Header{"Origin": []string{h.hs.URL}, "Cookie": []string{"vornik_session=abc"}}),
		"an agent request riding a browser session cookie is refused")
	assert.NoError(t, dial(http.Header{"Origin": []string{h.hs.URL}}), "same origin, header key: allowed")
	assert.NoError(t, dial(http.Header{}), "no Origin (a non-browser client): allowed")
}

// review-20261008-2892 finding 3, GitHub #92 (2026-10-08): operators keep the
// existing policy: a configured UI origin still upgrades for an operator key.
func TestExecutionLive_OperatorConfiguredOriginStillAllowed(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.srv.liveAllowedOrigins = []string{"ui.example.com"}
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, h.url("exec-a", ""), &websocket.DialOptions{HTTPHeader: http.Header{
		"Origin": []string{"https://ui.example.com"}, "Authorization": []string{"Bearer " + liveOperatorKey}}})
	require.NoError(t, err)
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// review-20261008-f680 minor, GitHub #92 (2026-10-08): the cursor grammar is
// unambiguous even for an execution id containing a colon (the seq is after
// the last one); generated ids are colon-free.
func TestParseLogCursor_ColonInExecutionID(t *testing.T) {
	id, seq, err := parseLogCursor("exec:odd:id:42")
	require.NoError(t, err)
	assert.Equal(t, "exec:odd:id", id)
	assert.EqualValues(t, 42, seq)
	assert.False(t, strings.Contains("exec_20261008101613_2eb67e21737ec6bc", ":"))
}

package service

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

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// writeServer is an MCP server (and a REST upstream on /api) that counts
// the writes it receives.
type writeServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
}

func newWriteServer(t *testing.T) *writeServer {
	ws := &writeServer{}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			ws.mu.Lock()
			ws.calls = append(ws.calls, r.Method+" "+r.URL.Path+" "+string(body))
			ws.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":7}`))
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read_inbox"}, map[string]any{"name": "send"}}}
		case "tools/call":
			ws.mu.Lock()
			ws.calls = append(ws.calls, "tool "+req.Params.Name)
			ws.mu.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "sent"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (ws *writeServer) seen() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]string(nil), ws.calls...)
}

func stageAction(t *testing.T, f *agentAdminFixture, id, tool, args string) {
	const project = "hermes--comms"
	t.Helper()
	ctx := context.Background()
	sum, _ := approval.CanonicalSHA256([]byte(args))
	now := time.Now().UTC()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: id, ProjectID: project, TaskID: "task_" + id,
		WorkflowID: project + "--reply", ActionKind: "send_mail", Tool: tool, ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_"+id); err != nil {
		t.Fatal(err)
	}
	f.c.notifyBrokerActionsPending(ctx, project, "task_"+id, 1)
}

func actionRequest(t *testing.T, f *agentAdminFixture, actionID string) *persistence.AgentApprovalRequestRow {
	t.Helper()
	r, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), "apr_ba_"+actionID)
	if err != nil {
		t.Fatalf("no phone approval was filed for %s: %v", actionID, err)
	}
	return r
}

// Plan P4.8 end to end: an agent project's proposed write waits for a tap
// on the phone; approving it executes it exactly once; rejecting it sends
// nothing; an action whose integration approval was removed is refused by
// the worker; an API write goes out with exactly its method and fixed path.
// The push sentence carries no arguments. Control: the action approval
// bridge and the worker's routing caller.
func TestAgentAdmin_ProposedWritesApprovedOnThePhone(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Broker.Writes = "on" })
	ctx := context.Background()
	ws := newWriteServer(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: ws.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "none"}, WriteTools: []string{"send"}}))
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "comms", Name: "pay", BaseURL: ws.srv.URL + "/api",
		Methods: []string{"GET", "POST"}, Writes: true}))
	f.c.brokerActionWorker = f.c.newBrokerActionWorker()
	if f.c.brokerActionWorker == nil {
		t.Fatal("no broker action worker")
	}

	// Approved: executes once.
	stageAction(t, f, "ba_1", "mcp__mail-write__send", `{"subject":"PRIVATE-SUBJECT","to":"a@b.example"}`)
	req := actionRequest(t, f, "ba_1")
	if req.Kind != persistence.ApprovalKindBrokerAction || strings.Contains(req.Sentence, "PRIVATE-SUBJECT") ||
		!strings.Contains(string(req.Rendered), "PRIVATE-SUBJECT") || !strings.Contains(req.Sentence, "send mail") {
		t.Fatalf("request %s %q / %s", req.Kind, req.Sentence, req.Rendered)
	}
	if err := f.c.approverDeviceService().Decide(ctx, f.device, req.ID, req.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_1")
	f.c.brokerActionWorker.Execute(ctx, "ba_1")
	if got := ws.seen(); len(got) != 1 || got[0] != "tool send" {
		t.Fatalf("after one approval the server saw %q", got)
	}

	// Rejected: nothing is sent.
	stageAction(t, f, "ba_2", "mcp__mail-write__send", `{"to":"c@d.example"}`)
	r2 := actionRequest(t, f, "ba_2")
	if err := f.c.approverDeviceService().Decide(ctx, f.device, r2.ID, r2.RenderedSHA256, false); err != nil {
		t.Fatal(err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_2")
	if a, _ := f.c.repos.BrokerActions.Get(ctx, "ba_2"); a.Status != persistence.BrokerActionRejected || len(ws.seen()) != 1 {
		t.Fatalf("a rejected action: %s, server saw %q", a.Status, ws.seen())
	}

	// An API write: exactly its method and path.
	stageAction(t, f, "ba_4", "api:pay:POST:/payments", `{"amount":5}`)
	r4 := actionRequest(t, f, "ba_4")
	if err := f.c.approverDeviceService().Decide(ctx, f.device, r4.ID, r4.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_4")
	if got := ws.seen(); len(got) != 2 || got[1] != `POST /api/payments {"amount":5}` {
		t.Fatalf("the API write: %q", got)
	}

	// The integration's approval removed after the tap: refused, not sent.
	stageAction(t, f, "ba_3", "mcp__mail-write__send", `{"to":"e@f.example"}`)
	r3 := actionRequest(t, f, "ba_3")
	if err := f.c.approverDeviceService().Decide(ctx, f.device, r3.ID, r3.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(ctx, "hermes--comms", "mail", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_3")
	if a, _ := f.c.repos.BrokerActions.Get(ctx, "ba_3"); a.Status != persistence.BrokerActionFailed || len(ws.seen()) != 2 {
		t.Fatalf("an action after its approval was removed: %s, server saw %q", a.Status, ws.seen())
	}

	// The API twin (review 20261002-52aa #5): an approved API write whose
	// approval was removed before it ran is refused, nothing sent.
	stageAction(t, f, "ba_5", "api:pay:POST:/payments", `{"amount":9}`)
	r5 := actionRequest(t, f, "ba_5")
	if err := f.c.approverDeviceService().Decide(ctx, f.device, r5.ID, r5.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(ctx, "hermes--comms", "pay", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_5")
	if a, _ := f.c.repos.BrokerActions.Get(ctx, "ba_5"); a.Status != persistence.BrokerActionFailed || len(ws.seen()) != 2 {
		t.Fatalf("an API write after its approval was removed: %s, server saw %q", a.Status, ws.seen())
	}
	// Filing again is a no-op (idempotent).
	f.c.notifyBrokerActionsPending(ctx, "hermes--comms", "task_ba_5", 1)
}

// Review 20261002-3ef1 R3: an agent action whose arguments carry a key at
// send time is refused at the worker's call seam, before anything is sent,
// even if staging was bypassed.
func TestActionCaller_ScansAtSend(t *testing.T) {
	f := newAgentAdminFixture(t)
	ws := newWriteServer(t)
	ac := actionCaller{c: f.c, mcp: f.c.mcpManager}
	_, _, err := ac.CallToolOnce(context.Background(), "hermes--comms", "api:pay:POST:/payments", `{"note":"AKIAQWERTYUIOPASDFGH"}`)
	if err == nil || !strings.Contains(err.Error(), "$.note") || strings.Contains(err.Error(), "AKIAQWERTYUIOPASDFGH") {
		t.Fatalf("a key at send time: %v", err)
	}
	if len(ws.seen()) != 0 {
		t.Fatal("something was sent")
	}
}

// Approval fatigue tier 1 (broker write-actions design, review 5c20,
// 2026-10-03): the writes one task drafted are filed as one group, described
// for the page, so the person reviews them together. One task holds at most
// one write per action kind (unique (task_id, action_kind)), so the group is
// the workflow's action across runs.
func TestAgentAdmin_AWorkflowsWritesAreOneGroup(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Broker.Writes = "on" })
	ctx := context.Background()
	const project = "hermes--comms"
	now := time.Now().UTC()
	for _, id := range []string{"ga_1", "ga_2", "ga_3"} {
		args := `{"to":"` + id + `@b.example"}`
		sum, _ := approval.CanonicalSHA256([]byte(args))
		if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: id, ProjectID: project, TaskID: "task_" + id,
			WorkflowID: project + "--reply", ActionKind: "send_mail", Tool: "mcp__mail-write__send", ArgsJSON: []byte(args), ArgsSHA256: sum,
			Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"ga_1", "ga_2", "ga_3"} {
		if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_"+id); err != nil {
			t.Fatal(err)
		}
		f.c.notifyBrokerActionsPending(ctx, project, "task_"+id, 1)
	}
	group := ""
	for _, id := range []string{"ga_1", "ga_2", "ga_3"} {
		req := actionRequest(t, f, id)
		d := f.c.approverDeviceService().Describe(*req)
		if d == nil || d.Group == "" || d.Level != agentadmin.LevelHigh || !strings.Contains(d.GroupTitle, "send mail") {
			t.Fatalf("%s: description %+v", id, d)
		}
		if group != "" && d.Group != group {
			t.Fatalf("one task's writes in two groups: %s and %s", group, d.Group)
		}
		group = d.Group
	}
}

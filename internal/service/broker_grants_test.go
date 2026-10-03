package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Standing grants end to end in an agent namespace — broker write-actions
// design, "Tier 2: standing grants" as revised, rounds 3 and 4, review 61a5:
// the phone approves a seed write with a grant; a second write to the same
// address is approved under it without a phone approval and executes; a
// write to another address files the ordinary per-write approval.

const grantArgsSchema = `{"type":"object","additionalProperties":false,"required":["to"],"properties":{"to":{"type":"string","format":"email","maxLength":254,"x-destination":true},"body":{"type":"string","maxLength":2000,"x-untrusted":true}}}`

func standingFixture(t *testing.T) (*agentAdminFixture, *writeServer) {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Broker.Writes = "on" })
	ws := newWriteServer(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: ws.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "none"}, WriteTools: []string{"send"}}))
	f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "comms", Slug: "reply", Purpose: "reply",
		Steps:    []agentadmin.StepInput{{Name: "draft", Role: "worker", Instructions: "Draft a reply."}},
		Egress:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`),
		Proposes: json.RawMessage(`[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":` + grantArgsSchema + `,"standing":{"key":["to"]}}]`)}))
	if f.c.Registry.GetWorkflow("hermes--comms--reply") == nil {
		t.Fatal("the proposing workflow did not load")
	}
	f.c.brokerActionWorker = f.c.newBrokerActionWorker()
	if f.c.brokerActionWorker == nil || f.c.brokerGrants() == nil {
		t.Fatal("the worker or the standing-grant service is not wired")
	}
	return f, ws
}

// stageReply stages and promotes one write of the reply workflow, as the
// executor does, and runs the promotion hook.
func stageReply(t *testing.T, f *agentAdminFixture, id, args string) {
	t.Helper()
	ctx := context.Background()
	sum, _ := approval.CanonicalSHA256([]byte(args))
	now := time.Now().UTC()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: id, ProjectID: "hermes--comms", TaskID: "task_" + id,
		WorkflowID: "hermes--comms--reply", ActionKind: "send_mail", Tool: "mcp__mail-write__send", ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_"+id); err != nil {
		t.Fatal(err)
	}
	f.c.notifyBrokerActionsPending(ctx, "hermes--comms", "task_"+id, 1)
}

func TestStandingGrants_AgentNamespaceEndToEnd(t *testing.T) {
	f, ws := standingFixture(t)
	ctx := context.Background()
	devices := f.c.approverDeviceService()

	// The seed: a per-write approval, approved with a grant.
	stageReply(t, f, "ba_seed", `{"to":"jana@example.com","body":"seed"}`)
	req := actionRequest(t, f, "ba_seed")
	if err := devices.DecideGrant(ctx, f.device, req.ID, req.RenderedSHA256, 7, 20); err != nil {
		t.Fatalf("approve with a grant: %v", err)
	}
	seed, _ := f.c.repos.BrokerActions.Get(ctx, "ba_seed")
	if seed.Status != persistence.BrokerActionApproved || seed.Approver != "device:"+f.device.ID {
		t.Fatalf("seed %s by %q", seed.Status, seed.Approver)
	}
	grants, err := f.c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: "hermes"})
	if err != nil || len(grants) != 1 || grants[0].UsesLeft != 20 || grants[0].Namespace != "hermes" {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_seed")

	// A second write to the same address: no phone approval, executes.
	stageReply(t, f, "ba_2", `{"to":"Jana <jana@example.com>","body":"second"}`)
	if _, err := f.c.repos.ApproverDevices.GetRequest(ctx, "apr_ba_ba_2"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a covered write filed a phone approval: %v", err)
	}
	a2, _ := f.c.repos.BrokerActions.Get(ctx, "ba_2")
	if a2.Status != persistence.BrokerActionApproved || a2.Approver != "grant:"+grants[0].ID {
		t.Fatalf("the covered write is %s by %q", a2.Status, a2.Approver)
	}
	f.c.brokerActionWorker.Execute(ctx, "ba_2")
	if got := ws.seen(); len(got) != 2 {
		t.Fatalf("the server saw %q, want the seed and the covered write", got)
	}
	if g, _ := f.c.repos.BrokerGrants.Get(ctx, grants[0].ID); g.UsesLeft != 19 {
		t.Fatalf("uses_left = %d", g.UsesLeft)
	}

	// Another recipient: the ordinary per-write approval.
	stageReply(t, f, "ba_3", `{"to":"eve@example.com","body":"x"}`)
	actionRequest(t, f, "ba_3")
	if a3, _ := f.c.repos.BrokerActions.Get(ctx, "ba_3"); a3.Status != persistence.BrokerActionPending {
		t.Fatalf("another recipient is %s", a3.Status)
	}

	// The Standing approvals page lists the grant and its covered write;
	// revoke is effective on the next write.
	views, err := f.c.standingDevicePages().List(ctx)
	if err != nil || len(views) != 1 || views[0].Key != "to jana@example.com" || len(views[0].Covered) != 1 {
		t.Fatalf("standing page = %+v, %v", views, err)
	}
	if err := f.c.standingDevicePages().Change(ctx, grants[0].ID, "revoke"); err != nil {
		t.Fatal(err)
	}
	stageReply(t, f, "ba_4", `{"to":"jana@example.com","body":"after revoke"}`)
	actionRequest(t, f, "ba_4")
}

// The page offers a grant only for an eligible write, with the action's own
// normalised key.
func TestStandingGrants_DeviceOfferIsTheActionsOwnKey(t *testing.T) {
	f, _ := standingFixture(t)
	stageReply(t, f, "ba_1", `{"to":"Jana <jana@Example.com>","body":"b"}`)
	req := actionRequest(t, f, "ba_1")
	o := f.c.grantOfferFor(context.Background(), *req)
	if o == nil || o.Key != "to jana@example.com" || len(o.Unreviewed) != 1 || o.Unreviewed[0] != "body" {
		t.Fatalf("offer = %+v", o)
	}
}

// The config and loader ceilings are one set of numbers (item 9).
func TestStandingGrants_CeilingsAgree(t *testing.T) {
	if config.StandingGrantCeilingDays != registry.StandingMaxDays || config.StandingGrantCeilingUses != registry.StandingMaxUses ||
		config.StandingGrantCeilingLive != registry.StandingMaxLivePerProject {
		t.Fatal("the daemon's standing-grant ceilings differ from the loader's")
	}
}

// Review 3bed item 6: the device's offer is for agent namespaces only. An
// operator project's write (decided in /inbox) is never offered a grant on
// the phone, even when its workflow declares standing.
func TestStandingGrants_DeviceOfferRefusesAnOperatorProject(t *testing.T) {
	f, _ := standingFixture(t)
	ctx := context.Background()
	args := `{"to":"jana@example.com","body":"b"}`
	sum, _ := approval.CanonicalSHA256([]byte(args))
	now := time.Now().UTC()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: "ba_op", ProjectID: "ops", TaskID: "task_ba_op",
		WorkflowID: "hermes--comms--reply", ActionKind: "send_mail", Tool: "mcp__mail-write__send", ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_ba_op"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(actionPayload{ActionID: "ba_op", Project: "ops", ArgsSHA256: sum})
	if o := f.c.grantOfferFor(ctx, persistence.AgentApprovalRequestRow{Kind: persistence.ApprovalKindBrokerAction, Rendered: raw}); o != nil {
		t.Fatalf("an operator project's write was offered a grant on the phone: %+v", o)
	}
}

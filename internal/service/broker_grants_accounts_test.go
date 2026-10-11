package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #119 item 8 (2026-10-08): a standing grant made for one account
// never sends from another (agent-administered design §22.2.6; Review Focus
// 2). A grant is bound to its workflow, action and tool (so to one
// connection, one account); removing the connection (C6) or replacing its
// credential (C7) suspends the grants that write through it; a write whose
// account cannot be confirmed is never covered; the Standing approvals page
// names the account.

// accountGrantFixture approves, in project comms, a write connection per
// (name, account) and a proposing workflow reply-<name> per connection, each
// with a standing key on "to".
func accountGrantFixture(t *testing.T, auth agentadmin.MCPAuthInput, conns map[string]*agentadmin.AccountInput) *agentAdminFixture {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Broker.Writes = "on" })
	ws := newWriteServer(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	for name, acct := range conns {
		a := auth
		if a.Mode == "static" {
			a.Credential = strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_TOKEN"
		}
		f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: name, URL: ws.srv.URL + "/mcp/" + name,
			Auth: a, WriteTools: []string{"send"}, Account: acct}))
		if a.Mode == "static" {
			// A credentialed server is listed, and its tools approved, once
			// its credential is entered.
			enterAccountCredential(t, f, a.Credential, "first")
			tools := pendingToolsRequest(t, f)
			if err := f.c.approverDeviceService().Decide(context.Background(), f.device, tools.ID, tools.RenderedSHA256, true); err != nil {
				t.Fatalf("approve tools: %v", err)
			}
		}
		f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "comms", Slug: "reply-" + name, Purpose: "reply",
			Steps:    []agentadmin.StepInput{{Name: "draft", Role: "worker", Instructions: "Draft a reply."}},
			Egress:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`),
			Proposes: json.RawMessage(`[{"action":"send_mail","tool":"mcp__` + name + `-write__send","args_schema":` + grantArgsSchema + `,"standing":{"key":["to"]}}]`)}))
	}
	f.c.brokerActionWorker = f.c.newBrokerActionWorker()
	return f
}

func enterAccountCredential(t *testing.T, f *agentAdminFixture, name, value string) {
	t.Helper()
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: name, Purpose: "send", Kind: "secret"})
	slot, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(res))
	if err != nil {
		t.Fatalf("slot: %+v %v", res, err)
	}
	if err := f.svc.enterCredential(context.Background(), f.device, *slot, slot.RenderedSHA256, []byte(value)); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
}

// stageWrite stages and promotes one write of reply-<conn>, as the executor
// does, and runs the promotion hook (cover, then the phone request).
func stageWrite(t *testing.T, f *agentAdminFixture, conn, id, args string) {
	t.Helper()
	ctx := context.Background()
	sum, _ := approval.CanonicalSHA256([]byte(args))
	now := time.Now().UTC()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: id, ProjectID: "hermes--comms", TaskID: "task_" + id,
		WorkflowID: "hermes--comms--reply-" + conn, ActionKind: "send_mail", Tool: "mcp__" + conn + "-write__send", ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_"+id); err != nil {
		t.Fatal(err)
	}
	f.c.notifyBrokerActionsPending(ctx, "hermes--comms", "task_"+id, 1)
}

// seedGrant approves one write of reply-<conn> with a standing grant and
// returns the grant.
func seedGrant(t *testing.T, f *agentAdminFixture, conn string) *persistence.BrokerStandingGrant {
	t.Helper()
	ctx := context.Background()
	stageWrite(t, f, conn, "ba_seed_"+conn, `{"to":"jana@example.com","body":"seed"}`)
	req := actionRequest(t, f, "ba_seed_"+conn)
	if err := f.c.approverDeviceService().DecideGrant(ctx, f.device, req.ID, req.RenderedSHA256, 7, 20); err != nil {
		t.Fatalf("approve with a grant: %v", err)
	}
	gs, err := f.c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: "hermes", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if g.WorkflowID == "hermes--comms--reply-"+conn {
			return g
		}
	}
	t.Fatalf("no grant for %s: %+v", conn, gs)
	return nil
}

func covered(t *testing.T, f *agentAdminFixture, id string) bool {
	t.Helper()
	a, err := f.c.repos.BrokerActions.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(a.Approver, "grant:")
}

func suspended(t *testing.T, f *agentAdminFixture, id string) bool {
	t.Helper()
	g, err := f.c.repos.BrokerGrants.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return g.SuspendedAt != nil
}

func TestStandingGrant_DoesNotMatchAnotherAccount(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{
		"mail-work": {Label: "work"}, "mail-personal": {Label: "personal"}})
	seedGrant(t, f, "mail-work")
	// The same address from the personal account: a per-write approval.
	stageWrite(t, f, "mail-personal", "ba_personal", `{"to":"jana@example.com","body":"x"}`)
	if covered(t, f, "ba_personal") {
		t.Fatal("a grant for the work account covered a write from the personal account")
	}
	actionRequest(t, f, "ba_personal")
	// The work account's own write is covered.
	stageWrite(t, f, "mail-work", "ba_work", `{"to":"jana@example.com","body":"y"}`)
	if !covered(t, f, "ba_work") {
		t.Fatal("the work account's grant did not cover its own write")
	}
}

func TestStandingGrant_PageShowsAccount(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	seedGrant(t, f, "mail-work")
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 || views[0].Account != "your work account (not verified)" {
		t.Fatalf("standing page = %+v, %v", views, err)
	}
}

// C6 (closes review f2f0 item 7): removing a connection suspends the grants
// that write through it, so a same-name re-add cannot inherit them.
func TestStandingGrant_SuspendedWhenItsConnectionIsRemoved(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{
		"mail-work": {Label: "work"}, "mail-personal": {Label: "personal"}})
	work := seedGrant(t, f, "mail-work")
	personal := seedGrant(t, f, "mail-personal")
	if res := f.do(agentadmin.VerbRemove, agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail-work"}); res.Effect != agentadmin.EffectApplied {
		t.Fatalf("remove: %+v", res)
	}
	if !suspended(t, f, work.ID) {
		t.Fatal("the removed connection's grant is still live")
	}
	if suspended(t, f, personal.ID) {
		t.Fatal("another connection's grant was suspended")
	}
}

// C7, static: a rotation suspends, before the new value is stored, the
// grants that write through the credential.
func TestStandingGrant_SuspendedOnStaticRotation(t *testing.T) {
	// The fixture entered the first value; the grant is made after it.
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "static"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	g := seedGrant(t, f, "mail-work")
	if suspended(t, f, g.ID) {
		t.Fatal("a grant made after the first entry is suspended")
	}
	enterAccountCredential(t, f, "MAIL_WORK_TOKEN", "second")
	if !suspended(t, f, g.ID) {
		t.Fatal("a rotated credential's grant is still live")
	}
	stageWrite(t, f, "mail-work", "ba_after", `{"to":"jana@example.com","body":"x"}`)
	if covered(t, f, "ba_after") {
		t.Fatal("a suspended grant covered a write")
	}
}

// The cover never sends a write whose account cannot be confirmed (the
// approval row and the project file disagree), and says so before any key
// comparison: the grant is not even suspended by the matcher.
func TestStandingGrant_CoverRefusesAnUnconfirmedAccount(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	g := seedGrant(t, f, "mail-work")
	path := filepath.Join(f.cfgDir, "configs", "projects", "hermes--comms.yaml")
	b, _ := os.ReadFile(path)
	edited := strings.ReplaceAll(string(b), `account: "work"`, `account: "personal"`)
	if edited == string(b) {
		t.Fatal("the edit did not apply")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatal(err)
	}
	stageWrite(t, f, "mail-work", "ba_unconfirmed", `{"to":"jana@example.com","body":"x"}`)
	if covered(t, f, "ba_unconfirmed") {
		t.Fatal("a write whose account could not be confirmed was covered")
	}
	if suspended(t, f, g.ID) {
		t.Fatal("the cover reached the key comparison; the account check must come first")
	}
	if _, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), "apr_ba_ba_unconfirmed"); errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("no per-write approval was filed")
	}
}

// GitHub #317/#119 item 8: a suspended standing grant must remember the
// mailbox originally approved, before a device can move it to another.
func TestStandingGrant_StoresItsAccount(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(fmt.Sprint(provider), func(t *testing.T) {
			f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work", Identity: "alice@corp.example"}})
			if provider {
				setStandingIdentity(t, f, "alice@corp.example")
			}
			g := seedGrant(t, f, "mail-work")
			want := ""
			if provider {
				want = "alice@corp.example"
			}
			if g.AccountLabel != "work" || g.AccountIdentity != want {
				t.Fatalf("grant account = %q/%q, want work/%q", g.AccountLabel, g.AccountIdentity, want)
			}
		})
	}
}
func setStandingIdentity(t *testing.T, f *agentAdminFixture, identity string) {
	t.Helper()
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(context.Background(), "hermes--comms", "mail-work", identity); err != nil {
		t.Fatal(err)
	}
}
func TestStandingGrant_ChangedAccountNeedsReaffirm(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	setStandingIdentity(t, f, "alice@corp.example")
	g := seedGrant(t, f, "mail-work")
	if _, err := f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	setStandingIdentity(t, f, "bob@corp.example")
	if err := f.c.standingDevicePages().Change(context.Background(), g.ID, "confirm"); !errors.Is(err, brokergrants.ErrAccountChanged) {
		t.Fatal("one-tap confirm accepted a changed mailbox")
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("refused confirm cleared suspension")
	}
}
func TestWriteAccountConfirmation_IgnoresIdentity(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	for _, identity := range []string{"alice@corp.example", "bob@corp.example", ""} {
		setStandingIdentity(t, f, identity)
		_, _, ok := f.c.writeAccount(context.Background(), "hermes--comms", "mcp__mail-work-write__send")
		if !ok {
			t.Fatalf("identity %q changed the account confirmation", identity)
		}
	}
}

func TestStandingGrant_PageShowsMadeForAndNow(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	setStandingIdentity(t, f, "alice@corp.example")
	g := seedGrant(t, f, "mail-work")
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	setStandingIdentity(t, f, "bob@corp.example")
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %+v, %v", views, err)
	}
	v := views[0]
	if v.MadeFor != "your work account (alice@corp.example)" || !strings.Contains(v.NowAccount, "bob@corp.example") || !v.AccountChanged || v.ReaffirmToken == "" {
		t.Fatalf("drift not shown: %+v", v)
	}
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), g.ID, v.ReaffirmToken); err != nil {
		t.Fatal(err)
	}
	got, _ := f.c.repos.BrokerGrants.Get(context.Background(), g.ID)
	if got.SuspendedAt != nil || got.AccountIdentity != "bob@corp.example" || got.AccountLabel != "work" {
		t.Fatalf("reaffirm failed: %+v", got)
	}
}
func TestStandingGrant_SameAccountOneTapConfirm(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	setStandingIdentity(t, f, "alice@corp.example")
	g := seedGrant(t, f, "mail-work")
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	if err := f.c.standingDevicePages().Change(context.Background(), g.ID, "confirm"); err != nil {
		t.Fatal(err)
	}
	if suspended(t, f, g.ID) {
		t.Fatal("same-account confirm stayed suspended")
	}
}
func TestReaffirm_StaleHashRefused(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	g := seedGrant(t, f, "mail-work")
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	setStandingIdentity(t, f, "bob@corp.example")
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatal(err)
	}
	setStandingIdentity(t, f, "carol@corp.example")
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), g.ID, views[0].ReaffirmToken); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("stale pair = %v", err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("stale pair cleared suspension")
	}
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), "missing", views[0].ReaffirmToken); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("missing = %v", err)
	}
}

func TestStandingGrant_LegacyGrantNeedsReaffirm(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	g := seedGrant(t, f, "mail-work")
	if _, err := f.c.DB.Exec(`UPDATE broker_standing_grants SET account_label='',account_identity='' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	if err := f.c.standingDevicePages().Change(context.Background(), g.ID, "confirm"); !errors.Is(err, brokergrants.ErrAccountChanged) {
		t.Fatalf("legacy confirm: %v", err)
	}
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 || views[0].MadeFor != "an account Vornik did not record" {
		t.Fatalf("legacy page: %+v %v", views, err)
	}
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), g.ID, views[0].ReaffirmToken); err != nil {
		t.Fatal(err)
	}
	got, _ := f.c.repos.BrokerGrants.Get(context.Background(), g.ID)
	if got.AccountLabel != "work" || got.AccountIdentity != "" || got.SuspendedAt != nil {
		t.Fatalf("legacy reaffirm: %+v", got)
	}
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), g.ID, views[0].ReaffirmToken); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("already confirmed: %v", err)
	}
}

type movedStandingGrant struct {
	persistence.BrokerGrantRepository
}

func (m movedStandingGrant) ConfirmAccountGuarded(context.Context, string, string, string, string, string, string, persistence.AccountDecisionGuard) error {
	return persistence.ErrBrokerGrantNoTransition
}
func TestReaffirm_ConditionalNoRowRefused(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": {Label: "work"}})
	g := seedGrant(t, f, "mail-work")
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	setStandingIdentity(t, f, "bob@corp.example")
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatal(err)
	}
	f.c.repos.BrokerGrants = movedStandingGrant{f.c.repos.BrokerGrants}
	if err := f.c.standingDevicePages().Reaffirm(context.Background(), g.ID, views[0].ReaffirmToken); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("concurrently moved stored account: %v", err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("conditional no-row cleared suspension")
	}
}

func TestStandingGrant_DefaultAccountPhrase(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail-work": nil})
	g := seedGrant(t, f, "mail-work")
	if _, err := f.c.DB.Exec(`UPDATE broker_standing_grants SET account_label='' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.c.repos.BrokerGrants.Suspend(context.Background(), g.ID, time.Now())
	views, err := f.c.standingDevicePages().List(context.Background())
	if err != nil || len(views) != 1 || views[0].NowAccount != "your default account (not verified)" {
		t.Fatalf("default reaffirm phrase: %+v %v", views, err)
	}
}

func (m movedStandingGrant) ConfirmGuarded(ctx context.Context, id, reach string, guard persistence.AccountDecisionGuard) error {
	return m.BrokerGrantRepository.(persistence.AccountGuardedGrants).ConfirmGuarded(ctx, id, reach, guard)
}

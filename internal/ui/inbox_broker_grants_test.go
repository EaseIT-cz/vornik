package ui

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/registry"
)

// Standing grants in /inbox for operator projects — broker write-actions
// design, "Tier 2, revised" items 2 and 8 ("an operator project uses them
// from /inbox the same way, with the operator as the person"): the card
// offers the grant under Approve; the POST goes through the same gate as a
// single approve and creates the grant with the seed approval; the inbox
// lists standing approvals with pause and revoke; an agent project's grant
// is not changed here.

type plainSealer struct{}

func (plainSealer) Seal(ns, label string, plain []byte) (string, error) {
	return "sv1:" + base64.StdEncoding.EncodeToString([]byte(ns+"|"+label+"|"+string(plain))), nil
}

func (plainSealer) Open(ns, label, sealed string) ([]byte, error) {
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "sv1:"))
	return []byte(strings.TrimPrefix(string(raw), ns+"|"+label+"|")), nil
}

type grantUI struct {
	srv     *Server
	actions persistence.BrokerActionRepository
	grants  persistence.BrokerGrantRepository
	kicked  []string
}

func newGrantUI(t *testing.T) *grantUI {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "ui.db"), ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var p registry.BrokerProposal
	if err := yaml.Unmarshal([]byte(`
action: send_reply
tool: mcp__mail__send
output: p.json
standing: {key: [to]}
args_schema:
  type: object
  additionalProperties: false
  properties:
    to: {type: string, format: email, maxLength: 254, x-destination: true}
    body: {type: string, maxLength: 2000, x-untrusted: true}
`), &p); err != nil {
		t.Fatal(err)
	}
	g := &grantUI{actions: sqlite.NewBrokerActionRepository(db.DB), grants: sqlite.NewBrokerGrantRepository(db.DB)}
	svc := brokergrants.New(brokergrants.Config{Grants: g.grants, Actions: g.actions,
		Proposal: func(wf, action string) (registry.BrokerProposal, bool) {
			return p, wf == "mail-reply" && action == "send_reply"
		},
		ReachHash:      func(context.Context, string, string) (string, error) { return "r1", nil },
		SealerForWrite: func() (brokergrants.Sealer, error) { return plainSealer{}, nil },
		SealerForRead:  func() (brokergrants.Sealer, error) { return plainSealer{}, nil },
		Metrics:        brokergrants.NewMetrics()})
	g.srv = NewServer(WithBrokerActions(g.actions, func(id string) { g.kicked = append(g.kicked, id) }),
		WithStandingGrants(func() *brokergrants.Service { return svc }))
	return g
}

func (g *grantUI) pending(t *testing.T, id, project, args string) *persistence.BrokerAction {
	t.Helper()
	sum, _ := approval.CanonicalSHA256([]byte(args))
	a := &persistence.BrokerAction{ActionID: id, ProjectID: project, TaskID: "task_" + id, WorkflowID: "mail-reply",
		ActionKind: "send_reply", Tool: "mcp__mail__send", ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if _, err := g.actions.Stage(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := g.actions.PromoteStaged(context.Background(), a.TaskID); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestInboxGrant_CardOffersTheGrant(t *testing.T) {
	g := newGrantUI(t)
	g.pending(t, "ba_1", "p1", `{"body":"hi","to":"Ada <ada@Example.com>"}`)
	rec := httptest.NewRecorder()
	g.srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	for _, want := range []string{"/ui/inbox/broker-action/ba_1/approve-grant", "to ada@example.com",
		"without showing you their text", `name="grant_days"`, `name="grant_uses"`, "body"} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox lacks %q", want)
		}
	}
}

func TestInboxGrant_ApproveWithGrantCreatesItAndCovers(t *testing.T) {
	g := newGrantUI(t)
	seed := g.pending(t, "ba_1", "p1", `{"body":"hi","to":"ada@example.com"}`)
	post := func(form url.Values, hdr string) int {
		req := brokerActionPost("/ui/inbox/broker-action/ba_1/approve-grant", form)
		if hdr != "" {
			req.Header.Set("Sec-Fetch-Site", hdr)
		}
		rec := httptest.NewRecorder()
		g.srv.brokerActionInboxRouter(rec, req)
		return rec.Code
	}
	if code := post(url.Values{"args_sha256": {seed.ArgsSHA256}, "grant_days": {"7"}, "grant_uses": {"20"}}, "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
	if code := post(url.Values{"args_sha256": {seed.ArgsSHA256}, "grant_days": {"30"}, "grant_uses": {"20"}}, ""); code != http.StatusBadRequest {
		t.Fatalf("a choice not offered: %d", code)
	}
	if code := post(url.Values{"args_sha256": {"stale"}, "grant_days": {"7"}, "grant_uses": {"20"}}, ""); code != http.StatusSeeOther {
		t.Fatalf("stale: %d", code)
	}
	if gs, _ := g.grants.List(context.Background(), persistence.BrokerGrantFilter{}); len(gs) != 0 {
		t.Fatal("a stale hash created a grant")
	}
	if code := post(url.Values{"args_sha256": {seed.ArgsSHA256}, "grant_days": {"7"}, "grant_uses": {"20"}}, ""); code != http.StatusSeeOther {
		t.Fatalf("approve-grant: %d", code)
	}
	gs, _ := g.grants.List(context.Background(), persistence.BrokerGrantFilter{})
	if len(gs) != 1 || gs[0].Namespace != "" || gs[0].CreatedBy == "" {
		t.Fatalf("grants = %+v", gs)
	}
	if a, _ := g.actions.Get(context.Background(), "ba_1"); a.Status != persistence.BrokerActionApproved || len(g.kicked) != 1 {
		t.Fatalf("seed %s, kicks %v", a.Status, g.kicked)
	}
	// The inbox lists the standing approval with pause and revoke.
	rec := httptest.NewRecorder()
	g.srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	for _, want := range []string{"Standing approvals", "/ui/inbox/standing/" + gs[0].ID + "/pause", "/ui/inbox/standing/" + gs[0].ID + "/revoke", "20 of 20"} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox lacks %q", want)
		}
	}
	// Revoke from /inbox.
	rec = httptest.NewRecorder()
	g.srv.StandingGrantChange(rec, brokerActionPost("/ui/inbox/standing/"+gs[0].ID+"/revoke", url.Values{}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if got, _ := g.grants.Get(context.Background(), gs[0].ID); got.Active {
		t.Fatal("not revoked")
	}
}

// An agent project's write is approved on the phone only, so is its grant:
// the inbox neither offers nor changes one.
func TestInboxGrant_AgentProjectsAreDeviceOnly(t *testing.T) {
	g := newGrantUI(t)
	a := g.pending(t, "ba_1", "hermes--comms", `{"to":"ada@example.com"}`)
	rec := httptest.NewRecorder()
	g.srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_1/approve-grant",
		url.Values{"args_sha256": {a.ArgsSHA256}, "grant_days": {"7"}, "grant_uses": {"20"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent project approve-grant: %d", rec.Code)
	}
	agentGrant := &persistence.BrokerStandingGrant{ID: "bsg_x", ProjectID: "hermes--comms", Namespace: "hermes", WorkflowID: "mail-reply",
		Action: "send_reply", KeyPaths: []string{"to"}, KeyValuesSealed: "sv1:x", KeyHash: "k", MaxUses: 5, UsesLeft: 5,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), CreatedBy: "device:d", SeedActionID: "ba_1",
		ReachHashAtCreation: "r1", Active: true, DigestThrough: time.Now()}
	if err := g.grants.ApproveSeedAndCreate(context.Background(), "ba_1", a.ArgsSHA256, "device:d", agentGrant, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	g.srv.StandingGrantChange(rec, brokerActionPost("/ui/inbox/standing/bsg_x/revoke", url.Values{}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent grant revoke from /inbox: %d", rec.Code)
	}
}

// Review 3bed item 6: the handler states its own agent-namespace refusal,
// not only through the shared row gate.
func TestInboxGrant_HandlerHasItsOwnAgentGuard(t *testing.T) {
	src, err := os.ReadFile("inbox_broker_grants.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (s *Server) BrokerActionApproveWithGrant(")
	j := strings.Index(body[i:], "svc.ApproveWithGrant(")
	if i < 0 || j < 0 || !strings.Contains(body[i:i+j], "agentns.FromID(row.ProjectID)") {
		t.Fatal("BrokerActionApproveWithGrant must refuse an agent-namespace project itself, before ApproveWithGrant")
	}
}

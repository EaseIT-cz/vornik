package brokergrants_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"

	"gopkg.in/yaml.v3"
)

// Standing grants, the service half — broker write-actions design, "Tier 2:
// standing grants" as revised (items 1 to 9), rounds 3 and 4, review 61a5.
// The store is a real SQLite file database with the daemon's DSN.

// fakeSealer binds a value to its namespace and label like the secret store
// (a value sealed for one label does not open for another).
type fakeSealer struct {
	mu     sync.Mutex
	opened []string // labels opened, for the non-observation checks
}

func (f *fakeSealer) Seal(ns, label string, plain []byte) (string, error) {
	return "sv1:" + base64.StdEncoding.EncodeToString([]byte(ns+"|"+label+"|"+string(plain))), nil
}

func (f *fakeSealer) Open(ns, label, sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "sv1:"))
	if err != nil {
		return nil, err
	}
	prefix := ns + "|" + label + "|"
	if !strings.HasPrefix(string(raw), prefix) {
		return nil, errors.New("does not open for this label")
	}
	f.mu.Lock()
	f.opened = append(f.opened, label)
	f.mu.Unlock()
	return raw[len(prefix):], nil
}

const mailProposal = `
action: send_reply
tool: mcp__mail-write__gmail_send
output: proposal.json
standing: { key: [to, cc] }
args_schema:
  type: object
  additionalProperties: false
  required: [to, body]
  properties:
    to:   { type: string, format: email, maxLength: 254, x-destination: true, x-untrusted: true }
    cc:   { type: array, maxItems: 5, items: { type: string, format: email, maxLength: 254 }, x-destination: true }
    body: { type: string, maxLength: 4000, x-untrusted: true }
`

type env struct {
	t       *testing.T
	ctx     context.Context
	svc     *brokergrants.Service
	grants  persistence.BrokerGrantRepository
	actions persistence.BrokerActionRepository
	sealer  *fakeSealer
	now     time.Time
	reach   map[string]string
	metrics *brokergrants.Metrics
	props   map[string]registry.BrokerProposal
	bounds  [3]int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.File(t, "g.db")
	var p registry.BrokerProposal
	if err := yamlUnmarshal(mailProposal, &p); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, grants: sqlite.NewBrokerGrantRepository(db.DB), actions: sqlite.NewBrokerActionRepository(db.DB),
		sealer: &fakeSealer{}, now: time.Now().UTC().Truncate(time.Second), reach: map[string]string{},
		metrics: brokergrants.NewMetrics(), props: map[string]registry.BrokerProposal{"ns1--mail/send_reply": p, "ns1--other/send_reply": p},
		bounds: [3]int{7, 20, 10}}
	e.svc = e.rebuild(e.grants)
	return e
}

// rebuild builds the service over grants (a test may wrap the store).
func (e *env) rebuild(grants persistence.BrokerGrantRepository) *brokergrants.Service {
	return brokergrants.New(brokergrants.Config{
		Grants: grants, Actions: e.actions,
		Proposal: func(wf, action string) (registry.BrokerProposal, bool) {
			p, ok := e.props[wf+"/"+action]
			return p, ok
		},
		ReachHash: func(_ context.Context, _, wf string) (string, error) {
			if h, ok := e.reach[wf]; ok {
				return h, nil
			}
			return "reach-1", nil
		},
		SealerForWrite: func() (brokergrants.Sealer, error) { return e.sealer, nil },
		SealerForRead:  func() (brokergrants.Sealer, error) { return e.sealer, nil },
		Bounds:         func() (int, int, int) { return e.bounds[0], e.bounds[1], e.bounds[2] },
		Metrics:        e.metrics,
		Now:            func() time.Time { return e.now },
	})
}

func (e *env) action(wf, args string) *persistence.BrokerAction {
	const project = "ns1--p"
	e.t.Helper()
	a := &persistence.BrokerAction{ActionID: fmt.Sprintf("bact_%d", time.Now().UnixNano()), ProjectID: project, TaskID: fmt.Sprintf("task_%d", time.Now().UnixNano()),
		WorkflowID: wf, ActionKind: "send_reply", Tool: "mcp__mail-write__gmail_send", ArgsJSON: []byte(args),
		ArgsSHA256: fmt.Sprintf("sha_%d", time.Now().UnixNano()), Status: persistence.BrokerActionStaged,
		CreatedAt: e.now, ExpiresAt: e.now.Add(24 * time.Hour)}
	if _, err := e.actions.Stage(e.ctx, a); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.actions.PromoteStaged(e.ctx, a.TaskID); err != nil {
		e.t.Fatal(err)
	}
	a.Status = persistence.BrokerActionPending
	return a
}

func (e *env) seed(args string, days, uses int) *persistence.BrokerStandingGrant {
	e.t.Helper()
	a := e.action("ns1--mail", args)
	g, err := e.svc.ApproveWithGrant(e.ctx, a, a.ArgsSHA256, "device:dev_1", days, uses)
	if err != nil {
		e.t.Fatalf("seed: %v", err)
	}
	return g
}

func (e *env) state(id string) *persistence.BrokerAction {
	e.t.Helper()
	a, err := e.actions.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// Item 1: an action without standing is never offered a grant, and never
// covered.
func TestOffer_OnlyForAStandingDeclaration(t *testing.T) {
	e := newEnv(t)
	p := e.props["ns1--mail/send_reply"]
	p.Standing = nil
	e.props["ns1--plain/send_reply"] = p
	a := e.action("ns1--plain", `{"to":"a@x.com","body":"b"}`)
	if o := e.svc.Offer(a); o != nil {
		t.Fatalf("an action without standing was offered %+v", o)
	}
	if covered, _ := e.svc.Cover(e.ctx, a); covered {
		t.Fatal("an action without standing was covered")
	}
	b := e.action("ns1--mail", `{"to":"Jana <jana@Example.com>","body":"b"}`)
	o := e.svc.Offer(b)
	if o == nil {
		t.Fatal("an eligible action was not offered a grant")
	}
	if o.Key != "to jana@example.com, cc (none)" || fmt.Sprint(o.Days) != "[1 7]" || fmt.Sprint(o.Uses) != "[5 20]" {
		t.Fatalf("offer = %+v", o)
	}
	if fmt.Sprint(o.Unreviewed) != "[body]" {
		t.Fatalf("the unreviewed fields = %v, want [body]", o.Unreviewed)
	}
	// The daemon's lowered bounds narrow the choice.
	e.bounds = [3]int{1, 5, 10}
	if o := e.svc.Offer(b); fmt.Sprint(o.Days) != "[1]" || fmt.Sprint(o.Uses) != "[5]" {
		t.Fatalf("lowered bounds offer %+v", o)
	}
}

// Items 2 and 9: the choice must be one offered; a stale hash creates
// nothing.
func TestApproveWithGrant_ChoicesAndStaleHash(t *testing.T) {
	e := newEnv(t)
	a := e.action("ns1--mail", `{"to":"a@x.com","body":"b"}`)
	for _, c := range [][2]int{{8, 5}, {7, 21}, {3, 5}, {7, 0}} {
		if _, err := e.svc.ApproveWithGrant(e.ctx, a, a.ArgsSHA256, "device:d", c[0], c[1]); !errors.Is(err, brokergrants.ErrNotOffered) {
			t.Fatalf("%v: %v, want ErrNotOffered", c, err)
		}
	}
	if _, err := e.svc.ApproveWithGrant(e.ctx, a, "stale", "device:d", 7, 20); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("stale: %v", err)
	}
	if st := e.state(a.ActionID); st.Status != persistence.BrokerActionPending {
		t.Fatalf("seed is %s", st.Status)
	}
	g, err := e.svc.ApproveWithGrant(e.ctx, a, a.ArgsSHA256, "device:d", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if g.UsesLeft != 5 || !g.ExpiresAt.Equal(e.now.Add(24*time.Hour)) || g.Namespace != "ns1" || g.CreatedBy != "device:d" ||
		strings.Contains(g.KeyValuesSealed, "a@x.com") {
		t.Fatalf("grant = %+v (key values must be sealed)", g)
	}
}

// Item 4 and the coverage tests: a matching later action is approved with
// grant:<id>; normalised keys match; another key, a plus-tag, another
// workflow, an added cc do not.
func TestCover_MatchesTheNormalisedKeyOnly(t *testing.T) {
	e := newEnv(t)
	g := e.seed(`{"to":"jana@example.com","body":"seed"}`, 7, 20)
	match := e.action("ns1--mail", `{"to":"Jana <jana@EXAMPLE.com> ","body":"later"}`)
	if covered, err := e.svc.Cover(e.ctx, match); err != nil || !covered {
		t.Fatalf("a normalised match was not covered: %v %v", covered, err)
	}
	if st := e.state(match.ActionID); st.Status != persistence.BrokerActionApproved || st.Approver != "grant:"+g.ID {
		t.Fatalf("covered action %s by %q", st.Status, st.Approver)
	}
	for _, args := range []string{
		`{"to":"jana+x@example.com","body":"b"}`,
		`{"to":"eve@example.com","body":"b"}`,
		`{"to":"jana@example.com","cc":["eve@example.com"],"body":"b"}`,
	} {
		a := e.action("ns1--mail", args)
		if covered, _ := e.svc.Cover(e.ctx, a); covered {
			t.Errorf("%s was covered", args)
		}
		if st := e.state(a.ActionID); st.Status != persistence.BrokerActionPending {
			t.Errorf("%s is %s", args, st.Status)
		}
	}
	other := e.action("ns1--other", `{"to":"jana@example.com","body":"b"}`)
	if covered, _ := e.svc.Cover(e.ctx, other); covered {
		t.Error("another workflow's action was covered")
	}
	if got := e.metrics.Covered("ns1--p"); got != 1 {
		t.Errorf("covered metric = %v, want 1", got)
	}
}

// Count reaching zero, expiry with no sweep, pause and revoke: the next
// action is a per-write approval, and the downgrade is counted by reason
// (round 4 F5).
func TestCover_StopsAndCountsTheMiss(t *testing.T) {
	e := newEnv(t)
	args := `{"to":"a@x.com","body":"b"}`
	g := e.seed(args, 1, 5)
	for i := 0; i < 5; i++ {
		if c, err := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); !c || err != nil {
			t.Fatalf("use %d: %v %v", i, c, err)
		}
	}
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); c {
		t.Fatal("covered after the last use")
	}
	if e.metrics.Miss("used") != 1 {
		t.Fatalf("miss{used} = %v", e.metrics.Miss("used"))
	}
	_ = g
	g2 := e.seed(`{"to":"b@x.com"}`, 1, 5)
	if err := e.svc.Pause(e.ctx, g2.ID, true); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"b@x.com"}`)); c || e.metrics.Miss("paused") != 1 {
		t.Fatal("a paused grant covered, or the miss was not counted")
	}
	if err := e.svc.Pause(e.ctx, g2.ID, false); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"b@x.com"}`)); !c {
		t.Fatal("unpause did not restore coverage")
	}
	if err := e.svc.Revoke(e.ctx, g2.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"b@x.com"}`)); c || e.metrics.Miss("revoked") != 1 {
		t.Fatal("a revoked grant covered")
	}
	// Expiry: a day later the 1-day grant no longer covers; nothing swept.
	g3 := e.seed(`{"to":"c@x.com"}`, 1, 5)
	e.now = g3.ExpiresAt
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"c@x.com"}`)); c || e.metrics.Miss("expired") != 1 {
		t.Fatal("an expired grant covered")
	}
	if e.metrics.Revoked() != 1 {
		t.Fatalf("revoked_total = %v", e.metrics.Revoked())
	}
}

// Item 6 and round 4 F4: a reach change suspends the grant on the next
// action (counted); confirming re-pins the hash and restores coverage.
func TestCover_ReachChangeSuspends(t *testing.T) {
	e := newEnv(t)
	args := `{"to":"a@x.com"}`
	g := e.seed(args, 7, 20)
	e.reach["ns1--mail"] = "reach-2"
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); c {
		t.Fatal("covered after the workflow's reach changed")
	}
	if e.metrics.Suspended() != 1 || e.metrics.Miss("suspended") != 1 {
		t.Fatalf("suspended_total %v, miss{suspended} %v", e.metrics.Suspended(), e.metrics.Miss("suspended"))
	}
	got, _ := e.grants.Get(e.ctx, g.ID)
	if got.SuspendedAt == nil {
		t.Fatal("not suspended")
	}
	if err := e.svc.Confirm(e.ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.grants.Get(e.ctx, g.ID); got.ReachHashAtCreation != "reach-2" {
		t.Fatalf("confirm pinned %q", got.ReachHashAtCreation)
	}
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); !c {
		t.Fatal("confirm did not restore coverage")
	}
}

// Item 7: two grants on one key never add; the sooner-expiring is used.
func TestCover_OverlapUsesTheSoonerExpiring(t *testing.T) {
	e := newEnv(t)
	args := `{"to":"a@x.com"}`
	long := e.seed(args, 7, 20)
	short := e.seed(args, 1, 5)
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); !c {
		t.Fatal("not covered")
	}
	l, _ := e.grants.Get(e.ctx, long.ID)
	s, _ := e.grants.Get(e.ctx, short.ID)
	if s.UsesLeft != 4 || l.UsesLeft != 20 {
		t.Fatalf("short %d, long %d; want the sooner-expiring used", s.UsesLeft, l.UsesLeft)
	}
}

// Review 61a5 F1: creation and decrement hash one key through one function;
// the hash stored at creation is the one the decrement recomputes.
func TestKeyHash_CreationAndDecrementAgree(t *testing.T) {
	e := newEnv(t)
	g := e.seed(`{"to":"Jana <jana@Example.com>","cc":["b@x.com","a@x.com"],"body":"x"}`, 7, 20)
	p := e.props["ns1--mail/send_reply"]
	k, err := brokergrants.KeyOf(p.Standing.Key, p.Destinations(), []byte(`{"to":"jana@example.com","cc":["a@x.com","b@x.com"],"body":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if k.Hash != g.KeyHash {
		t.Fatalf("creation stored %s, the decrement path computes %s", g.KeyHash, k.Hash)
	}
}

// The digest counts each covered action once (item 8): a second pass over
// the same window pushes nothing.
func TestDigest_CountsEachCoveredActionOnce(t *testing.T) {
	e := newEnv(t)
	var pushes []string
	e.svc.SetNotify(func(_ context.Context, ns string, n int) { pushes = append(pushes, fmt.Sprintf("%s:%d", ns, n)) })
	args := `{"to":"a@x.com"}`
	e.seed(args, 7, 20)
	e.now = e.now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", args)); !c {
			t.Fatal("not covered")
		}
	}
	e.now = e.now.Add(24 * time.Hour)
	if err := e.svc.Digest(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Digest(e.ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pushes) != "[ns1:3]" {
		t.Fatalf("pushes = %v, want one push of 3", pushes)
	}
}

// Round 3 F7, round 4 F6, review 61a5 F3: key values never reach the agent.
// The agent observes only its action's state (and whether a per-write
// approval was filed). Over a population of UNCOVERED keys that surface is
// identical whatever the key; separately, over keys each covered by one
// grant, it is identical too. Key identity is the only variable.
func TestNonObservation_TheAgentSurfaceDoesNotDependOnTheKey(t *testing.T) {
	e := newEnv(t)
	e.seed(`{"to":"granted@x.com"}`, 7, 20)
	keys := []string{"a@x.com", "B@y.org", "c+tag@x.com", "Name <d@x.com>", "e@granted.com"}
	surface := func(a *persistence.BrokerAction, covered bool) string {
		st := e.state(a.ActionID)
		approver := "person"
		if st.Approver != "" {
			approver = "grant" // the agent never sees an approver; only that no request was filed
		}
		return fmt.Sprintf("%s/%v/%s", st.Status, covered, map[bool]string{true: approver, false: "-"}[st.Approver != ""])
	}
	var uncovered []string
	for _, k := range keys {
		a := e.action("ns1--mail", fmt.Sprintf(`{"to":%q}`, k))
		c, err := e.svc.Cover(e.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		uncovered = append(uncovered, surface(a, c))
	}
	for _, s := range uncovered[1:] {
		if s != uncovered[0] {
			t.Fatalf("uncovered keys gave different surfaces: %v", uncovered)
		}
	}
	var coveredS []string
	for _, k := range keys {
		e.seed(fmt.Sprintf(`{"to":%q}`, k), 7, 20)
		a := e.action("ns1--mail", fmt.Sprintf(`{"to":%q}`, k))
		c, _ := e.svc.Cover(e.ctx, a)
		coveredS = append(coveredS, surface(a, c))
	}
	for _, s := range coveredS[1:] {
		if s != coveredS[0] {
			t.Fatalf("covered keys gave different surfaces: %v", coveredS)
		}
	}
	if coveredS[0] == uncovered[0] {
		t.Fatal("the populations are not covered/uncovered as built")
	}
}

// The grant page's view opens the key (the second of the two openers).
func TestViews_ShowTheKeyAndCoveredActions(t *testing.T) {
	e := newEnv(t)
	g := e.seed(`{"to":"a@x.com"}`, 7, 20)
	if c, _ := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"a@x.com"}`)); !c {
		t.Fatal("not covered")
	}
	views, err := e.svc.Views(e.ctx, persistence.BrokerGrantFilter{Namespace: "ns1"})
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %d %v", len(views), err)
	}
	v := views[0]
	if v.Grant.ID != g.ID || v.Key != "to a@x.com, cc (none)" || len(v.Covered) != 1 || v.State != "active" {
		t.Fatalf("view = %+v", v)
	}
}

func yamlUnmarshal(s string, v any) error { return yaml.Unmarshal([]byte(s), v) }

package approverdevice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
)

// Standing grants on the approver device — broker write-actions design,
// "Tier 2, revised" items 2 and 8: the page offers, under an eligible
// write's Approve, "Approve, and send future <action> to <key> without
// showing you their text, for [1 | 7] days, at most [5 | 20] times"; the
// choice is recorded with the decision and read by the effect; and a
// Standing approvals page lists every grant with Pause and Revoke.

func brokerActionRequest(t *testing.T, f *fixture, id string) persistence.AgentApprovalRequestRow {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"action_id": "bact_" + id, "args": map[string]any{"to": "a@x.com", "body": "hi"}})
	canon, _ := approval.Canonical(raw)
	sum, _ := approval.CanonicalSHA256(canon)
	r := persistence.AgentApprovalRequestRow{ID: id, Namespace: "ns1", Kind: persistence.ApprovalKindBrokerAction,
		Sentence: "Your assistant (ns1) wants to send reply.", Rendered: canon, RenderedSHA256: sum,
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(time.Hour)}
	if err := f.svc.FileRequest(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

type effects struct {
	mu   sync.Mutex
	seen []string
}

func (e *effects) effect(_ context.Context, r persistence.AgentApprovalRequestRow) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, r.ID+"="+r.DecidedChoice)
	return nil
}

func grantFixture(t *testing.T) (*fixture, *effects, *browser) {
	t.Helper()
	f := newFixture(t)
	e := &effects{}
	f.svc.RegisterEffect(persistence.ApprovalKindBrokerAction, e.effect)
	f.svc.RegisterGrantOffer(persistence.ApprovalKindBrokerAction, func(_ context.Context, r persistence.AgentApprovalRequestRow) *GrantOffer {
		if r.ID == "apr_plain" {
			return nil
		}
		return &GrantOffer{Action: "send reply", Key: "to a@x.com", Days: []int{1, 7}, Uses: []int{5, 20}, Unreviewed: []string{"body"}}
	})
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	return f, e, phone
}

func TestGrantOffer_PageStatesTheConcession(t *testing.T) {
	f, _, phone := grantFixture(t)
	brokerActionRequest(t, f, "apr_g1")
	brokerActionRequest(t, f, "apr_plain")
	_, page := phone.do(http.MethodGet, "/ui/approve/apr_g1", nil, nil)
	for _, want := range []string{
		"Approve, and send future send reply to to a@x.com without showing you their text",
		`name="grant_days"`, `value="1"`, `value="7"`, `name="grant_uses"`, `value="5"`, `value="20"`,
		`value="approve_grant"`, "will not be shown to you: body", "may reach several people",
		`value="approve"`, `value="reject"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	_, plain := phone.do(http.MethodGet, "/ui/approve/apr_plain", nil, nil)
	if strings.Contains(plain, "approve_grant") {
		t.Error("a write without an offer shows the grant form")
	}
}

func TestGrantOffer_DecisionCarriesTheChoice(t *testing.T) {
	f, e, phone := grantFixture(t)
	r := brokerActionRequest(t, f, "apr_g1")
	post := func(v url.Values) int {
		v.Set("rendered_sha256", r.RenderedSHA256)
		res, _ := phone.do(http.MethodPost, "/ui/approve/apr_g1", v, nil)
		return res.StatusCode
	}
	// A choice not offered decides nothing.
	for _, bad := range []url.Values{
		{"decision": {"approve_grant"}, "grant_days": {"3"}, "grant_uses": {"5"}},
		{"decision": {"approve_grant"}, "grant_days": {"7"}, "grant_uses": {"21"}},
		{"decision": {"approve_grant"}},
	} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", bad, code)
		}
	}
	if got, _ := f.repo.GetRequest(context.Background(), "apr_g1"); got.Status != persistence.ApprovalPending {
		t.Fatalf("a refused choice decided: %s", got.Status)
	}
	if code := post(url.Values{"decision": {"approve_grant"}, "grant_days": {"7"}, "grant_uses": {"20"}}); code != http.StatusSeeOther {
		t.Fatalf("approve_grant: %d", code)
	}
	got, _ := f.repo.GetRequest(context.Background(), "apr_g1")
	if got.Status != persistence.ApprovalApproved || got.DecidedChoice != GrantChoice(7, 20) {
		t.Fatalf("decided %s with choice %q", got.Status, got.DecidedChoice)
	}
	if len(e.seen) != 1 || e.seen[0] != "apr_g1=grant:7:20" {
		t.Fatalf("effect saw %v", e.seen)
	}
	if d, u, ok := ParseGrantChoice("grant:7:20"); !ok || d != 7 || u != 20 {
		t.Fatal("ParseGrantChoice")
	}
	for _, bad := range []string{"", "once", "grant:7", "grant:x:1", "grant:0:5", "grant:7:0"} {
		if _, _, ok := ParseGrantChoice(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A plain approve stays a plain approve: no choice recorded.
func TestGrantOffer_PlainApproveRecordsNoChoice(t *testing.T) {
	f, e, phone := grantFixture(t)
	r := brokerActionRequest(t, f, "apr_g1")
	res, _ := phone.do(http.MethodPost, "/ui/approve/apr_g1", url.Values{"decision": {"approve"}, "rendered_sha256": {r.RenderedSHA256}}, nil)
	if res.StatusCode != http.StatusSeeOther || len(e.seen) != 1 || e.seen[0] != "apr_g1=" {
		t.Fatalf("plain approve: %d, effect saw %v", res.StatusCode, e.seen)
	}
}

type fakeStanding struct {
	mu      sync.Mutex
	changes []string
}

func (f *fakeStanding) List(context.Context) ([]StandingView, error) {
	exp := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	return []StandingView{{ID: "bsg_1", Namespace: "ns1", Workflow: "mail", Action: "send reply", Key: "to a@x.com",
		State: "active", UsesLeft: 18, MaxUses: 20, ExpiresAt: exp,
		Covered: []StandingCovered{{ActionID: "bact_9", Status: "executed", At: exp.Add(-time.Hour)}}}}, nil
}

func (f *fakeStanding) Change(_ context.Context, id, verb string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "bsg_1" {
		return ErrNotDecidable
	}
	f.changes = append(f.changes, id+":"+verb)
	return nil
}

func TestStandingPage_ListsAndChanges(t *testing.T) {
	f, _, phone := grantFixture(t)
	st := &fakeStanding{}
	f.svc.SetStandingPages(st)
	res, page := phone.do(http.MethodGet, "/ui/approve/standing", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("standing page: %d", res.StatusCode)
	}
	for _, want := range []string{"Standing approvals", "to a@x.com", "18 of 20 uses left", "9 Oct 2026 09:00 UTC",
		"bact_9", "executed", `action="/ui/approve/standing/bsg_1/pause"`, `action="/ui/approve/standing/bsg_1/revoke"`,
		"sent without showing you their text"} {
		if !strings.Contains(page, want) {
			t.Errorf("standing page lacks %q", want)
		}
	}
	for _, verb := range []string{"pause", "unpause", "revoke", "confirm"} {
		if res, _ := phone.do(http.MethodPost, "/ui/approve/standing/bsg_1/"+verb, url.Values{}, nil); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d", verb, res.StatusCode)
		}
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/standing/bsg_1/delete", url.Values{}, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown verb: %d", res.StatusCode)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/standing/bsg_1/revoke", url.Values{}, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site revoke: %d", res.StatusCode)
	}
	if strings.Join(st.changes, ",") != "bsg_1:pause,bsg_1:unpause,bsg_1:revoke,bsg_1:confirm" {
		t.Fatalf("changes = %v", st.changes)
	}
	// Without a device, nothing.
	anon := newBrowser(t, phone.srv)
	if res, _ := anon.do(http.MethodGet, "/ui/approve/standing", nil, nil); res.StatusCode == http.StatusOK {
		t.Fatal("the standing page answered without a device")
	}
}

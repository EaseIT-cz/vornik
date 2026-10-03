package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// Broker-action approval in /inbox — design 2026-09-29 §5.3 and §13
// ("Approval: GET does nothing; a cross-site POST is refused; an args_sha256
// mismatch is refused; a project-scoped caller cannot approve another
// project's action; approve on a non-pending row mints nothing").

// uiFakeBrokerActionRepo implements the approval-relevant half of
// persistence.BrokerActionRepository; the rest is unused by the UI.
type uiFakeBrokerActionRepo struct {
	persistence.BrokerActionRepository
	mu      sync.Mutex
	rows    map[string]*persistence.BrokerAction
	approve []string // "id|shown|approver"
	reject  []string
}

func newUIFakeBrokerActionRepo(rows ...*persistence.BrokerAction) *uiFakeBrokerActionRepo {
	r := &uiFakeBrokerActionRepo{rows: map[string]*persistence.BrokerAction{}}
	for _, a := range rows {
		cp := *a
		r.rows[a.ActionID] = &cp
	}
	return r
}

func (r *uiFakeBrokerActionRepo) Get(_ context.Context, id string) (*persistence.BrokerAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (r *uiFakeBrokerActionRepo) ListByStatus(_ context.Context, projectID, status string, _ int) ([]*persistence.BrokerAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*persistence.BrokerAction
	for _, a := range r.rows {
		if a.Status == status && (projectID == "" || a.ProjectID == projectID) {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

// Approve mirrors the store's guard: pending, same hash, unexpired.
func (r *uiFakeBrokerActionRepo) Approve(_ context.Context, id, shown, approver string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approve = append(r.approve, id+"|"+shown+"|"+approver)
	a, ok := r.rows[id]
	if !ok || a.Status != persistence.BrokerActionPending || a.ArgsSHA256 != shown || !a.ExpiresAt.After(now) {
		return persistence.ErrBrokerActionNoTransition
	}
	a.Status = persistence.BrokerActionApproved
	return nil
}

func (r *uiFakeBrokerActionRepo) Reject(_ context.Context, id, approver string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reject = append(r.reject, id+"|"+approver)
	a, ok := r.rows[id]
	if !ok || (a.Status != persistence.BrokerActionPending && a.Status != persistence.BrokerActionApproved) {
		return persistence.ErrBrokerActionNoTransition
	}
	a.Status = persistence.BrokerActionRejected
	return nil
}

func pendingBrokerActionFixture() *persistence.BrokerAction {
	return &persistence.BrokerAction{
		ActionID: "ba_test_1", ProjectID: "p1", TaskID: "task_1", APIKeyID: "key_1",
		WorkflowID: "mail-reply", ActionKind: "send_reply", Tool: "mcp__mail__send",
		ArgsJSON:   []byte(`{"body":"Thanks, see you Tuesday.","to":"ada@example.com"}`),
		ArgsSHA256: "hash_shown",
		Status:     persistence.BrokerActionPending,
		CreatedAt:  time.Now().Add(-2 * time.Minute),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	}
}

func brokerActionPost(target string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func brokerActionServer(repo *uiFakeBrokerActionRepo, kicked *[]string) *Server {
	return NewServer(WithBrokerActions(repo, func(id string) { *kicked = append(*kicked, id) }))
}

func TestBrokerActionApprove_GETDoesNothing(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	req := httptest.NewRequest(http.MethodGet, "/ui/inbox/broker-action/ba_test_1/approve?args_sha256=hash_shown", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve = %d, want 405", rec.Code)
	}
	if len(repo.approve) != 0 || len(kicked) != 0 {
		t.Fatalf("GET reached the store (%v) or the worker (%v)", repo.approve, kicked)
	}
}

func TestBrokerActionApprove_CrossSiteRefused(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	req := brokerActionPost("/ui/inbox/broker-action/ba_test_1/approve", url.Values{"args_sha256": {"hash_shown"}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site approve = %d, want 403", rec.Code)
	}
	if len(repo.approve) != 0 || len(kicked) != 0 {
		t.Fatal("cross-site approve reached the store or the worker")
	}
}

func TestBrokerActionApprove_BindsToTheShownHashAndKicks(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/approve",
		url.Values{"args_sha256": {"hash_shown"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d, want 303", rec.Code)
	}
	if len(repo.approve) != 1 || !strings.HasPrefix(repo.approve[0], "ba_test_1|hash_shown|") {
		t.Fatalf("store Approve calls = %v, want one with the shown hash", repo.approve)
	}
	if len(kicked) != 1 || kicked[0] != "ba_test_1" {
		t.Fatalf("worker kicks = %v, want [ba_test_1]", kicked)
	}
}

func TestBrokerActionApprove_HashMismatchIsRefusedAndNotKicked(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	for _, shown := range []string{"hash_of_other_args", ""} {
		rec := httptest.NewRecorder()
		srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/approve",
			url.Values{"args_sha256": {shown}}))
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "broker-action-not-approvable") {
			t.Errorf("shown=%q: Location = %q, want the not-approvable notice", shown, loc)
		}
	}
	if len(kicked) != 0 {
		t.Fatalf("a refused approve kicked the worker: %v", kicked)
	}
	if a, _ := repo.Get(context.Background(), "ba_test_1"); a.Status != persistence.BrokerActionPending {
		t.Fatalf("status = %s, want pending", a.Status)
	}
}

func TestBrokerActionApprove_NonPendingIsRefused(t *testing.T) {
	row := pendingBrokerActionFixture()
	row.Status = persistence.BrokerActionRejected
	repo := newUIFakeBrokerActionRepo(row)
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/approve",
		url.Values{"args_sha256": {"hash_shown"}}))
	if len(kicked) != 0 {
		t.Fatalf("approve of a rejected action kicked the worker: %v", kicked)
	}
}

func TestBrokerActionApprove_UnknownIDIs404(t *testing.T) {
	var kicked []string
	srv := brokerActionServer(newUIFakeBrokerActionRepo(), &kicked)
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/nope/approve", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", rec.Code)
	}
}

func TestBrokerActionReject_POSTRejectsAndNeverKicks(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/reject", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject = %d, want 303", rec.Code)
	}
	if len(repo.reject) != 1 || len(repo.approve) != 0 || len(kicked) != 0 {
		t.Fatalf("reject: store reject=%v approve=%v kicks=%v", repo.reject, repo.approve, kicked)
	}
}

func TestBrokerActionRoutes_UnconfiguredIs503(t *testing.T) {
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/approve", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d, want 503", rec.Code)
	}
}

func TestInbox_RendersPendingBrokerActionCard(t *testing.T) {
	pending := pendingBrokerActionFixture()
	done := pendingBrokerActionFixture()
	done.ActionID, done.Status = "ba_done", persistence.BrokerActionExecuted
	var kicked []string
	srv := brokerActionServer(newUIFakeBrokerActionRepo(pending, done), &kicked)

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"ba_test_1", "send_reply", "mcp__mail__send", "task_1",
		"ada@example.com", "Thanks, see you Tuesday.", // the complete arguments
		`name="args_sha256" value="hash_shown"`, // approve binds to what was rendered
		"/ui/inbox/broker-action/ba_test_1/approve",
		"/ui/inbox/broker-action/ba_test_1/reject",
		"third-party content",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox body missing %q", want)
		}
	}
	if strings.Contains(body, "ba_done") {
		t.Error("an executed action rendered as a pending card")
	}
}

// TestBrokerActionHandlers_UseTheSharedGate is the §13 "one gate" scan for
// the broker-action handlers: both reach approval.CheckRequest and
// approval.Authorize through brokerActionGate.
func TestBrokerActionHandlers_UseTheSharedGate(t *testing.T) {
	src, err := os.ReadFile("inbox_broker_actions.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{"approval.CheckRequest(r)", "approval.Authorize(r, row.ProjectID"} {
		if strings.Count(s, want) != 1 {
			t.Errorf("inbox_broker_actions.go: want exactly one %q (in brokerActionGate)", want)
		}
	}
	if strings.Count(s, "s.brokerActionGate(w, r, actionID)") != 2 {
		t.Error("approve and reject must both go through brokerActionGate")
	}
	// The batch (approval fatigue tier 1) uses the same two halves.
	if strings.Count(s, "s.brokerRequestGate(w, r)") != 2 || strings.Count(s, "s.brokerRowGate(r, actionID)") != 2 {
		t.Error("the gate and the batch must share brokerRequestGate and brokerRowGate")
	}
}

func TestBrokerActionApprove_OtherProjectsCallerIsRefused(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture()) // project p1
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	for _, action := range []string{"approve", "reject"} {
		req := brokerActionPost("/ui/inbox/broker-action/ba_test_1/"+action, url.Values{"args_sha256": {"hash_shown"}})
		req = req.WithContext(api.ContextWithProjectScope(req.Context(), "p2"))
		rec := httptest.NewRecorder()
		srv.brokerActionInboxRouter(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
			t.Errorf("%s by a p2-scoped caller = %d, want 404/403", action, rec.Code)
		}
	}
	if len(repo.approve)+len(repo.reject) != 0 || len(kicked) != 0 {
		t.Fatal("a caller scoped to another project reached the store or the worker")
	}
}

func TestInbox_BrokerActionCardHiddenFromOtherProjects(t *testing.T) {
	var kicked []string
	srv := brokerActionServer(newUIFakeBrokerActionRepo(pendingBrokerActionFixture()), &kicked)
	req := httptest.NewRequest(http.MethodGet, "/ui/inbox", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "p2"))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, req)
	if strings.Contains(rec.Body.String(), "ba_test_1") {
		t.Fatal("a p2-scoped inbox rendered p1's broker action")
	}
}

type uiFakeAPIKeyNames struct {
	persistence.APIKeyRepository
}

func (uiFakeAPIKeyNames) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	if id == "key_1" {
		return &persistence.APIKey{ID: id, Name: "hermes-laptop"}, nil
	}
	return nil, persistence.ErrNotFound
}

// §5.3: the card names the front agent's key and flags the fields a model
// drafted from third-party content, read from the workflow's args_schema.
func TestInbox_BrokerActionCardNamesTheKeyAndFlagsUntrustedFields(t *testing.T) {
	reg := registry.New()
	if err := reg.RegisterTransient("mail-reply", &registry.Workflow{Broker: &registry.WorkflowBroker{
		Proposes: []registry.BrokerProposal{{
			Action: "send_reply", Tool: "mcp__mail__send", Output: "reply.json",
			ArgsSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"to":   map[string]any{"type": "string", "format": "email", "maxLength": 254},
				"body": map[string]any{"type": "string", "maxLength": 4000, "x-untrusted": true},
			}},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	var kicked []string
	srv := NewServer(
		WithBrokerActions(newUIFakeBrokerActionRepo(pendingBrokerActionFixture()), func(id string) { kicked = append(kicked, id) }),
		WithAPIKeyRepository(uiFakeAPIKeyNames{}),
		WithProjectRegistry(reg),
	)
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "requested by hermes-laptop") {
		t.Error("card does not name the front agent's key")
	}
	if !strings.Contains(body, `free text in: <span class="font-mono">body</span>`) {
		t.Error("card does not flag the x-untrusted body field")
	}
}

func TestUIFakeBrokerActionRepo_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "BrokerActionRepository.Get", newUIFakeBrokerActionRepo().Get)
}

// review-20260930-1334 F4: an expired row the sweep has not reached yet is
// not approvable, so it gets no card.
func TestInbox_ExpiredPendingActionHasNoCard(t *testing.T) {
	row := pendingBrokerActionFixture()
	row.ExpiresAt = time.Now().Add(-time.Minute)
	var kicked []string
	srv := brokerActionServer(newUIFakeBrokerActionRepo(row), &kicked)
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if strings.Contains(rec.Body.String(), "ba_test_1") {
		t.Fatal("an expired action rendered as an approval card")
	}
}

// review-20260930-1334 F2: when the workflow no longer declares the action,
// the card says which fields are free text is unknown, instead of flagging
// nothing and reading as "no untrusted fields".
func TestInbox_BrokerActionCardSaysWhenTheSchemaIsUnavailable(t *testing.T) {
	var kicked []string
	srv := NewServer(
		WithBrokerActions(newUIFakeBrokerActionRepo(pendingBrokerActionFixture()), func(id string) { kicked = append(kicked, id) }),
		WithProjectRegistry(registry.New()), // mail-reply is not loaded
	)
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if !strings.Contains(rec.Body.String(), "no longer declares this action") {
		t.Fatal("card does not disclose that the workflow's schema is unavailable")
	}
}

// Design §7a: a decision in /inbox tells the push outbox, so the front agent
// hears approved/rejected at once. Refused decisions change nothing and do
// not signal.
func TestBrokerActionDecisions_SignalThePushOutbox(t *testing.T) {
	repo := newUIFakeBrokerActionRepo(pendingBrokerActionFixture())
	other := pendingBrokerActionFixture()
	other.ActionID = "ba_test_2"
	repo.rows["ba_test_2"] = other
	var kicked []string
	changes := 0
	srv := NewServer(WithBrokerActions(repo, func(id string) { kicked = append(kicked, id) }),
		WithBrokerActionChanged(func() { changes++ }))
	post := func(path string, form url.Values) {
		rec := httptest.NewRecorder()
		srv.brokerActionInboxRouter(rec, brokerActionPost(path, form))
	}
	post("/ui/inbox/broker-action/ba_test_1/approve", url.Values{"args_sha256": {"wrong"}})
	if changes != 0 {
		t.Fatal("a refused approve signalled a change")
	}
	post("/ui/inbox/broker-action/ba_test_1/approve", url.Values{"args_sha256": {"hash_shown"}})
	post("/ui/inbox/broker-action/ba_test_2/reject", nil)
	if changes != 2 {
		t.Fatalf("changes = %d, want 2 (approve, reject)", changes)
	}
	if len(kicked) != 1 {
		t.Fatalf("worker kicks = %v, want only the approve", kicked)
	}
}

// Agent-administered Vornik plan P4.8: an agent project's action is approved
// on the approver device only; /inbox refuses to decide it either way.
// Control: the agent-project refusal in brokerActionGate.
func TestBrokerActionInbox_RefusesAgentProjectActions(t *testing.T) {
	a := pendingBrokerActionFixture()
	a.ProjectID = "hermes--comms"
	repo := newUIFakeBrokerActionRepo(a)
	var kicked []string
	srv := brokerActionServer(repo, &kicked)
	for _, verb := range []string{"approve", "reject"} {
		rec := httptest.NewRecorder()
		srv.brokerActionInboxRouter(rec, brokerActionPost("/ui/inbox/broker-action/ba_test_1/"+verb, url.Values{"args_sha256": {"hash_shown"}}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s of an agent action from /inbox = %d, want 403", verb, rec.Code)
		}
	}
	if len(repo.approve) != 0 || len(repo.reject) != 0 || len(kicked) != 0 {
		t.Fatalf("/inbox reached the store (%v %v) or the worker (%v)", repo.approve, repo.reject, kicked)
	}
	if cards := srv.loadPendingBrokerActions(httptest.NewRequest(http.MethodGet, "/ui/inbox", nil)); len(cards) != 0 {
		t.Fatalf("/inbox lists an agent action: %+v", cards)
	}
}

// Approval fatigue tier 1 (broker write-actions design, review 5c20,
// 2026-10-03): /inbox decides several ticked writes in one POST, each bound
// to the hash its card showed; a stale one is refused and named, the others
// proceed; a cross-site POST decides nothing.
func TestBrokerActionBatch_EachBoundToItsHash(t *testing.T) {
	a := pendingBrokerActionFixture()
	b := pendingBrokerActionFixture()
	b.ActionID, b.ArgsSHA256 = "ba_test_2", "hash_b"
	repo := newUIFakeBrokerActionRepo(a, b)
	var kicked []string
	srv := brokerActionServer(repo, &kicked)

	cross := brokerActionPost("/ui/inbox/broker-actions/batch", url.Values{"decision": {"approve"}, "pick": {"ba_test_1|hash_shown"}})
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.BrokerActionBatch(rec, cross)
	if rec.Code != http.StatusForbidden || len(repo.approve) != 0 {
		t.Fatalf("cross-site batch: %d, store %v", rec.Code, repo.approve)
	}

	rec = httptest.NewRecorder()
	srv.BrokerActionBatch(rec, brokerActionPost("/ui/inbox/broker-actions/batch", url.Values{
		"decision": {"approve"}, "pick": {"ba_test_1|hash_shown", "ba_test_2|stale"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("batch = %d", rec.Code)
	}
	if len(kicked) != 1 || kicked[0] != "ba_test_1" {
		t.Fatalf("kicked %v, want [ba_test_1]", kicked)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "ba_test_2") {
		t.Fatalf("the refused write is not named: %s", loc)
	}
}

package api

// GitHub #669 item 3 (design 22, §22.2.16b): the account-transition recovery
// door is admin class only; the list shows the full owner and whether this
// daemon holds it, and states how many rows it examined; every recover call
// is audited with the before-values, a refused one with its reason. Found
// at: production (2026-10-28, recovered by hand in SQL). Regression test at:
// integration (the HTTP handler). Lower than e2e-lane because the recovery
// logic below the handler is pinned on a real database in the service tests.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

type fakeRecoverer669 struct {
	rows     []AccountTransitionView
	examined int
	err      error
	listErr  error
	calls    int
}

func (f *fakeRecoverer669) ListBusy(context.Context) ([]AccountTransitionView, int, error) {
	return f.rows, f.examined, f.listErr
}

func (f *fakeRecoverer669) Recover(_ context.Context, scope string, epoch int64, owner string) (persistence.AccountTransitionRow, error) {
	f.calls++
	if f.err != nil {
		return persistence.AccountTransitionRow{}, f.err
	}
	return persistence.AccountTransitionRow{Scope: scope, Epoch: epoch, Busy: true, Owner: owner, Operation: "connection-config", StartedAt: "2026-10-28T15:00:00Z"}, nil
}

func recoveryServer669(f *fakeRecoverer669, audit persistence.AdminAuditRepository) *Server {
	s := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{journalAdminKey}}), WithAccountTransitionRecovery(f))
	s.adminAuditRepo = audit
	return s
}

func TestAccountTransitions669_AdminClassOnly(t *testing.T) {
	f := &fakeRecoverer669{}
	s := recoveryServer669(f, &stubAdminAuditRepo{})
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/operator/account-transitions", ""},
		{http.MethodPost, "/api/v1/operator/account-transitions/recover", `{"scope":"ns:a","epoch":1,"owner":"x"}`},
	} {
		rec := httptest.NewRecorder()
		s.OperatorAccountTransitions(rec, journalReq(c.method, c.path, c.body, "sk-vornik-not-admin"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a non-admin key: %d", c.method, c.path, rec.Code)
		}
	}
	if f.calls != 0 {
		t.Error("a non-admin key reached the recovery")
	}
}

func TestAccountTransitions669_ListShowsTheFullOwner(t *testing.T) {
	owner := "0123456789abcdef0123456789abcdef"
	s := recoveryServer669(&fakeRecoverer669{examined: 3, rows: []AccountTransitionView{{Scope: "ns:hermes", Epoch: 4, Owner: owner, Operation: "connection-config", HeldHere: true}}}, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.OperatorAccountTransitions(rec, journalReq(http.MethodGet, "/api/v1/operator/account-transitions", "", journalAdminKey))
	var out struct {
		Rows []struct {
			Owner    string `json:"owner"`
			HeldHere bool   `json:"heldHere"`
		} `json:"rows"`
		Examined int `json:"examined"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Rows) != 1 || out.Rows[0].Owner != owner || !out.Rows[0].HeldHere || out.Examined != 3 {
		t.Fatalf("list = %s (err %v)", rec.Body.String(), err)
	}
}

func TestAccountTransitions669_RecoverIsAudited(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	s := recoveryServer669(&fakeRecoverer669{}, audit)
	rec := httptest.NewRecorder()
	s.OperatorAccountTransitions(rec, journalReq(http.MethodPost, "/api/v1/operator/account-transitions/recover",
		`{"scope":"ns:hermes","epoch":4,"owner":"deadbeef"}`, journalAdminKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"epochAfter":5`) {
		t.Fatalf("recover: %d %s", rec.Code, rec.Body.String())
	}
	if len(audit.rows) != 2 || audit.rows[0].Action != "account_transition.recover_requested" || audit.rows[1].Action != "account_transition.recover" || audit.rows[1].Target != "ns:hermes" {
		t.Fatalf("audit = %+v", audit.rows)
	}
	for _, want := range []string{`"owner":"deadbeef"`, `"epoch_before":4`, `"epoch_after":5`, `"operation":"connection-config"`, `"started_at":"2026-10-28T15:00:00Z"`} {
		if !strings.Contains(audit.rows[1].After, want) {
			t.Errorf("the audit record lacks %s: %s", want, audit.rows[1].After)
		}
	}
}

func TestAccountTransitions669_RefusalsAreAuditedAnd409(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{ErrTransitionHeldHere, "TRANSITION_HELD"},
		{&JournalOpenError{IDs: []string{"caj_1"}}, "JOURNAL_OPEN"},
		{persistence.ErrAccountTransition, "NO_MATCH"},
	} {
		audit := &stubAdminAuditRepo{}
		s := recoveryServer669(&fakeRecoverer669{err: c.err}, audit)
		rec := httptest.NewRecorder()
		s.OperatorAccountTransitions(rec, journalReq(http.MethodPost, "/api/v1/operator/account-transitions/recover",
			`{"scope":"ns:hermes","epoch":4,"owner":"x"}`, journalAdminKey))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s: %d %s", c.code, rec.Code, rec.Body.String())
		}
		if len(audit.rows) != 2 || !strings.Contains(audit.rows[1].After, `"refused"`) {
			t.Errorf("%s: the refusal is not audited: %+v", c.code, audit.rows)
		}
	}
}

type failingAudit669 struct{ stubAdminAuditRepo }

func (*failingAudit669) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return errors.New("audit store down")
}

// Code review 7546 F2: no recovery runs whose request cannot be audited.
func TestAccountTransitions669_NoAuditNoRecovery(t *testing.T) {
	f := &fakeRecoverer669{}
	s := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{journalAdminKey}}), WithAccountTransitionRecovery(f))
	s.adminAuditRepo = &failingAudit669{}
	rec := httptest.NewRecorder()
	s.OperatorAccountTransitions(rec, journalReq(http.MethodPost, "/api/v1/operator/account-transitions/recover",
		`{"scope":"ns:hermes","epoch":4,"owner":"x"}`, journalAdminKey))
	if rec.Code != http.StatusServiceUnavailable || f.calls != 0 {
		t.Fatalf("status %d, recover calls %d; want 503 and none", rec.Code, f.calls)
	}
}

// The door's other branches (code review 4b78 N4).
func TestAccountTransitions669_OtherBranches(t *testing.T) {
	s := recoveryServer669(&fakeRecoverer669{}, &stubAdminAuditRepo{})
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/v1/operator/account-transitions", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/operator/account-transitions/recover", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/operator/account-transitions/elsewhere", "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/operator/account-transitions/recover", `{"scope":"","epoch":1,"owner":"x"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/operator/account-transitions/recover", `{"scope":"ns:a","epoch":-1,"owner":"x"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		s.OperatorAccountTransitions(rec, journalReq(c.method, c.path, c.body, journalAdminKey))
		if rec.Code != c.want {
			t.Errorf("%s %s %s: %d, want %d", c.method, c.path, c.body, rec.Code, c.want)
		}
	}
	unwired := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{journalAdminKey}}))
	rec := httptest.NewRecorder()
	unwired.OperatorAccountTransitions(rec, journalReq(http.MethodGet, "/api/v1/operator/account-transitions", "", journalAdminKey))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("an unwired door: %d, want 503", rec.Code)
	}
}

// A list error answers a fixed message; the detail stays in the log (7546 F7).
func TestAccountTransitions669_ListErrorIsAFixedMessage(t *testing.T) {
	s := recoveryServer669(&fakeRecoverer669{listErr: errors.New("pq: relation secret_table does not exist")}, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.OperatorAccountTransitions(rec, journalReq(http.MethodGet, "/api/v1/operator/account-transitions", "", journalAdminKey))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret_table") {
		t.Fatalf("list error: %d %s", rec.Code, rec.Body.String())
	}
}

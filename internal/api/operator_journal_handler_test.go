package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #285 (2026-10-08): the journal resolve verb (config-apply journal
// design §12). Admin class only, audited, errors mapped.

type fakeJournalApplier struct {
	fakeApplier
	resolveErr error
	resolved   []string // "id mode actor"
	open       []*persistence.ConfigApplyJournal
}

func (f *fakeJournalApplier) ListOpenJournal(context.Context) ([]*persistence.ConfigApplyJournal, error) {
	return f.open, nil
}

func (f *fakeJournalApplier) ResolveJournal(_ context.Context, id, mode, actor string) (*controlplane.ResolveResult, error) {
	f.resolved = append(f.resolved, id+" "+mode+" "+actor)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return &controlplane.ResolveResult{JournalID: id, Mode: mode, State: persistence.JournalStateApplied, Changed: true,
		ProposalStatus: persistence.ProposalStatusApplied, AffectedProjects: []string{"hermes--global-news"}}, nil
}

const journalAdminKey = "sk-vornik-operator-admin"

func journalServer(fa *fakeJournalApplier, audit persistence.AdminAuditRepository) *Server {
	s := NewServer(WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{journalAdminKey}}))
	s.proposalApplier = fa
	s.adminAuditRepo = audit
	return s
}

func journalReq(method, path, body, key string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	return req.WithContext(ctx)
}

func TestOperatorJournal_ResolveIsAdminClassOnly(t *testing.T) {
	fa := &fakeJournalApplier{}
	s := journalServer(fa, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep"}`, "sk-vornik-not-admin"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ADMIN_SCOPE_REQUIRED") {
		t.Fatalf("a non-admin key: %d %s", rec.Code, rec.Body.String())
	}
	if len(fa.resolved) != 0 {
		t.Fatal("the engine was called for a non-admin key")
	}
	rec = httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodGet, "/api/v1/operator/journal", ``, "sk-vornik-not-admin"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("list with a non-admin key: %d", rec.Code)
	}
}

func TestOperatorJournal_ResolveAuditsAndReturnsTheOutcome(t *testing.T) {
	fa := &fakeJournalApplier{}
	audit := &stubAdminAuditRepo{}
	s := journalServer(fa, audit)
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep","actor":"vadim"}`, journalAdminKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if len(fa.resolved) != 1 || fa.resolved[0] != "caj_1 keep vadim" {
		t.Fatalf("engine calls = %v", fa.resolved)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["state"] != "APPLIED" || out["changed"] != true {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if len(audit.rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.rows))
	}
	row := audit.rows[0]
	if row.Action != "config_journal.resolve" || row.Target != "caj_1" || !strings.HasPrefix(row.Principal, "api_key_sha256:") ||
		!strings.Contains(row.After, `"mode":"keep"`) || !strings.Contains(row.After, `"actor":"vadim"`) {
		t.Fatalf("audit row = %+v", row)
	}
}

func TestOperatorJournal_ResolveErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"not drift", controlplane.ErrJournalNotDrift, http.StatusConflict, "JOURNAL_NOT_DRIFT"},
		{"bad mode", controlplane.ErrResolveMode, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"unknown", persistence.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{"busy", controlplane.ErrApplyInProgress, http.StatusConflict, "APPLY_IN_PROGRESS"},
		{"fenced", controlplane.ErrWriterFenced, http.StatusConflict, "WRITER_FENCED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			audit := &stubAdminAuditRepo{}
			s := journalServer(&fakeJournalApplier{resolveErr: c.err}, audit)
			rec := httptest.NewRecorder()
			s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep"}`, journalAdminKey))
			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.code) {
				t.Fatalf("%v: %d %s, want %d %s", c.err, rec.Code, rec.Body.String(), c.want, c.code)
			}
			// A refusal that reached the engine is audited too.
			if len(audit.rows) != 1 || !strings.Contains(audit.rows[0].After, `"refused"`) {
				t.Fatalf("audit rows = %+v, want one refused row", audit.rows)
			}
		})
	}
}

func TestOperatorJournal_RejectsABadActor(t *testing.T) {
	fa := &fakeJournalApplier{}
	s := journalServer(fa, &stubAdminAuditRepo{})
	for _, actor := range []string{strings.Repeat("a", 121), "line\\nbreak"} {
		rec := httptest.NewRecorder()
		s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep","actor":"`+actor+`"}`, journalAdminKey))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("actor %q: %d", actor, rec.Code)
		}
	}
	if len(fa.resolved) != 0 {
		t.Fatal("the engine was called with a bad actor")
	}
}

type failingJournalAudit struct{ stubAdminAuditRepo }

func (*failingJournalAudit) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return errors.New("audit store down")
}

// A failed audit write is reported, never hidden; the resolution stands.
func TestOperatorJournal_FailedAuditIsReported(t *testing.T) {
	s := journalServer(&fakeJournalApplier{}, &failingJournalAudit{})
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"restore"}`, journalAdminKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auditError":"audit store down"`) {
		t.Fatalf("resolve with a failing audit: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOperatorJournal_ListOpenRows(t *testing.T) {
	fa := &fakeJournalApplier{open: []*persistence.ConfigApplyJournal{{ID: "caj_1", ProposalID: "cpp_1",
		State: persistence.JournalStateDrift, AffectedProjects: []string{"p"}, Failure: "drift"}}}
	s := journalServer(fa, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodGet, "/api/v1/operator/journal", ``, journalAdminKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"caj_1"`) || !strings.Contains(rec.Body.String(), `"state":"DRIFT"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOperatorJournal_NotWired(t *testing.T) {
	s := journalServer(nil, &stubAdminAuditRepo{})
	s.proposalApplier = &fakeApplier{} // no journal methods
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodGet, "/api/v1/operator/journal", ``, journalAdminKey))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: %d", rec.Code)
	}
}

// GitHub #301 (2026-10-08): --keep on a row whose proposal was REJECTED
// answered 500 RESOLVE_FAILED. Every refusal is now a 409 with a remedy,
// and a missing proposal no longer reads as a missing journal row (404).
func TestOperatorJournal_RefusalsAre409WithRemedy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"proposal missing", &controlplane.ResolveRefusal{Code: controlplane.RefusalProposalMissing, Reason: "gone",
			Remedy: "none in the verb", Err: persistence.ErrNotFound}, controlplane.RefusalProposalMissing},
		{"content changed", &controlplane.ResolveRefusal{Code: controlplane.RefusalProposalContentChanged, Reason: "digest",
			Remedy: "none in the verb"}, controlplane.RefusalProposalContentChanged},
		{"root mismatch", &controlplane.ResolveRefusal{Code: controlplane.RefusalRootMismatch, Reason: "root",
			Remedy: "use the right daemon", Err: controlplane.ErrJournalRootMismatch}, controlplane.RefusalRootMismatch},
		{"proposal moved", &controlplane.ResolveRefusal{Code: controlplane.RefusalProposalChanged, Reason: "moved",
			Remedy: "retry", Err: persistence.ErrProposalNotApproved}, controlplane.RefusalProposalChanged},
		// audit 2026-10-12, finding B: --restore on an APPLIED proposal.
		{"restore applied", &controlplane.ResolveRefusal{Code: controlplane.RefusalRestoreApplied, Reason: "applied",
			Remedy: "resolve with --keep, then rollback"}, controlplane.RefusalRestoreApplied},
		// The exact #301 error, should it ever escape the engine bare: not a 500.
		{"bare not approved", errors.Join(errors.New("journal DRIFT→APPLIED"), persistence.ErrProposalNotApproved),
			controlplane.RefusalProposalChanged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := journalServer(&fakeJournalApplier{resolveErr: c.err}, &stubAdminAuditRepo{})
			rec := httptest.NewRecorder()
			s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep"}`, journalAdminKey))
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.code) || !strings.Contains(rec.Body.String(), "Remedy:") {
				t.Fatalf("%v: %d %s, want 409 %s with a remedy", c.err, rec.Code, rec.Body.String(), c.code)
			}
		})
	}
	// The pre-existing sentinels carry a remedy too.
	for _, err := range []error{controlplane.ErrJournalNotDrift, controlplane.ErrApplyInProgress, controlplane.ErrWriterFenced} {
		s := journalServer(&fakeJournalApplier{resolveErr: err}, &stubAdminAuditRepo{})
		rec := httptest.NewRecorder()
		s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep"}`, journalAdminKey))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Remedy:") {
			t.Fatalf("%v: %d %s, want 409 with a remedy", err, rec.Code, rec.Body.String())
		}
	}
}

type keptJournalApplier struct{ fakeJournalApplier }

func (f *keptJournalApplier) ResolveJournal(_ context.Context, id, mode, _ string) (*controlplane.ResolveResult, error) {
	return &controlplane.ResolveResult{JournalID: id, ProposalID: "cpp_1", Mode: mode, State: persistence.JournalStateFailed,
		ProposalStatus: persistence.ProposalStatusRejected, ProposalStatusBefore: persistence.ProposalStatusRejected,
		Changed: true, Outcome: controlplane.ResolveOutcomeKeptUnapproved, Message: "Kept the files as they are.",
		Note: "resolved keep by vadim: operator kept files of a REJECTED proposal; the proposal stays REJECTED; drift"}, nil
}

// The kept-unapproved resolution is audited with the actor, the outcome
// and the note, and the response says what happened.
func TestOperatorJournal_KeptUnapprovedIsAuditedWithNote(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	s := journalServer(nil, audit)
	s.proposalApplier = &keptJournalApplier{}
	rec := httptest.NewRecorder()
	s.OperatorJournal(rec, journalReq(http.MethodPost, "/api/v1/operator/journal/caj_1/resolve", `{"mode":"keep","actor":"vadim"}`, journalAdminKey))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"kept_unapproved"`) ||
		!strings.Contains(rec.Body.String(), `"message":"Kept the files as they are."`) {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if len(audit.rows) != 1 {
		t.Fatalf("audit rows = %d", len(audit.rows))
	}
	after := audit.rows[0].After
	for _, want := range []string{`"actor":"vadim"`, `"outcome":"kept_unapproved"`, `"proposal_status_before":"REJECTED"`,
		`operator kept files of a REJECTED proposal`} {
		if !strings.Contains(after, want) {
			t.Errorf("audit After %s lacks %s", after, want)
		}
	}
}

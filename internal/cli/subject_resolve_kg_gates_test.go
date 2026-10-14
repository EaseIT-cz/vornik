package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/subjectresolve"
)

// GitHub #345 / GDPR LLD §4.17.9 (2026-10-13): `vornikctl subject resolve-kg`
// checked no verified request, applied no Art 18 screen, and wrote no audit
// row — the residual §4.17.5 recorded when the console page shipped with all
// three. These pin the CLI to the same gates, through the same
// internal/subjectresolve functions the page calls.

type rkgLog struct{ events []string }

func (l *rkgLog) add(e string) { l.events = append(l.events, e) }

func (l *rkgLog) index(e string) int {
	for i, x := range l.events {
		if x == e {
			return i
		}
	}
	return -1
}

type rkgRequests struct {
	reqs map[string]datasubject.Request
}

func (f *rkgRequests) GetRequest(_ context.Context, id string) (datasubject.Request, error) {
	r, ok := f.reqs[id]
	if !ok {
		return datasubject.Request{}, persistence.ErrNotFound
	}
	return r, nil
}

type rkgSubjects struct {
	subjects map[string]datasubject.Subject
}

func (f *rkgSubjects) GetSubject(_ context.Context, id string) (datasubject.Subject, error) {
	s, ok := f.subjects[id]
	if !ok {
		return datasubject.Subject{}, persistence.ErrNotFound
	}
	return s, nil
}

type rkgKG struct {
	log       *rkgLog
	cands     []datasubject.KGCandidate
	candErr   error
	bindErr   error
	bindState datasubject.KGBindingState
	binds     []string
}

func (f *rkgKG) Candidates(context.Context, string, []string, []string) ([]datasubject.KGCandidate, error) {
	f.log.add("candidates")
	return f.cands, f.candErr
}

func (f *rkgKG) Bind(_ context.Context, req datasubject.KGBindRequest) (datasubject.KGBindResult, error) {
	f.log.add("bind:" + req.EntityID)
	f.binds = append(f.binds, req.EntityID)
	if f.bindErr != nil {
		return datasubject.KGBindResult{EntityID: req.EntityID, State: f.bindState}, f.bindErr
	}
	return datasubject.KGBindResult{EntityID: req.EntityID, LinksAdded: 3}, nil
}

type rkgScreen struct {
	restricted map[string]bool
	err        error
}

func (f *rkgScreen) RestrictedEntities(_ context.Context, ids []string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, id := range ids {
		if f.restricted[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *rkgScreen) RestrictedChunks(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

type rkgAudit struct {
	log  *rkgLog
	rows []*persistence.AdminAuditEntry
	err  error
	// failAfter fails every write after the first N (0 = never).
	failAfter int
}

func (f *rkgAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	if f.err != nil {
		return f.err
	}
	if f.failAfter > 0 && len(f.rows) >= f.failAfter {
		return errors.New("audit disk full")
	}
	f.log.add("audit:" + e.Action)
	f.rows = append(f.rows, e)
	return nil
}

const (
	rkgSentinelName  = "Zyxwvu Sentinelperson"
	rkgSentinelNick  = "Qqsentinelnick"
	rkgSentinelOther = "Otherperson Sentinelname"
)

type rkgFixture struct {
	log    *rkgLog
	kg     *rkgKG
	screen *rkgScreen
	audit  *rkgAudit
	out    *bytes.Buffer
	deps   resolveKGDeps
}

func newRKGFixture(state datasubject.RequestState) *rkgFixture {
	log := &rkgLog{}
	f := &rkgFixture{
		log: log,
		kg: &rkgKG{log: log, cands: []datasubject.KGCandidate{
			{Entity: datasubject.KGEntity{ID: "ent_free", ProjectID: "p1", CanonicalName: rkgSentinelName},
				MatchedOn: rkgSentinelName, MentionCount: 4, State: datasubject.KGUnbound},
			{Entity: datasubject.KGEntity{ID: "ent_restricted", ProjectID: "p1", CanonicalName: "Restricted " + rkgSentinelName},
				MatchedOn: rkgSentinelName, MentionCount: 2, State: datasubject.KGBoundHere},
		}},
		screen: &rkgScreen{restricted: map[string]bool{"ent_restricted": true}},
		audit:  &rkgAudit{log: log},
		out:    &bytes.Buffer{},
	}
	f.deps = resolveKGDeps{
		svc: &subjectresolve.Service{
			Requests: &rkgRequests{reqs: map[string]datasubject.Request{
				"dsr_1": {ID: "dsr_1", SubjectID: "ds_1", Kind: datasubject.RequestErasure, State: state},
			}},
			KG: f.kg, Screen: f.screen, Audit: f.audit,
		},
		subjects: &rkgSubjects{subjects: map[string]datasubject.Subject{
			"ds_1": {ID: "ds_1", DisplayName: rkgSentinelName},
		}},
		out:   f.out,
		actor: subjectresolve.Actor{Principal: "cli:tester", Source: "cli"},
	}
	return f
}

func (f *rkgFixture) run(opts resolveKGOpts) error {
	if opts.subjectID == "" {
		opts.subjectID = "ds_1"
	}
	return runResolveKG(context.Background(), f.deps, opts)
}

func TestResolveKG_RefusesWithoutARequest(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	err := f.run(resolveKGOpts{})
	if err == nil || !strings.Contains(err.Error(), "--request") {
		t.Fatalf("err = %v, want a refusal naming --request", err)
	}
	if len(f.log.events) != 0 {
		t.Fatalf("something ran without a request: %v", f.log.events)
	}
}

func TestResolveKG_RefusesUnknownOrUnverifiedRequest(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		state    datasubject.RequestState
		want     string
	}{
		{"unknown", "dsr_nope", datasubject.StateVerified, "no such request"},
		{"open", "dsr_1", datasubject.StateOpen, "verified request"},
		{"refused", "dsr_1", datasubject.StateRefused, "verified request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRKGFixture(tc.state)
			err := f.run(resolveKGOpts{requestID: tc.id})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(f.log.events) != 0 {
				t.Fatalf("searched or audited for a request that may not produce data: %v", f.log.events)
			}
		})
	}
}

func TestResolveKG_RefusesSubjectMismatch(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	err := f.run(resolveKGOpts{requestID: "dsr_1", subjectID: "ds_other"})
	if err == nil || !strings.Contains(err.Error(), "ds_other") {
		t.Fatalf("err = %v, want a mismatch refusal", err)
	}
	if len(f.log.events) != 0 {
		t.Fatalf("ran for a mismatched subject: %v", f.log.events)
	}
}

func TestResolveKG_RefusesShortName(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1", names: []string{"ab"}}); err == nil {
		t.Fatal("a 2-character --name was accepted; it matches nearly every entity")
	}
	if len(f.log.events) != 0 {
		t.Fatalf("ran with an invalid name: %v", f.log.events)
	}
}

func TestResolveKG_AuditFailureRefusesBeforeAnyRead(t *testing.T) {
	for _, entities := range [][]string{nil, {"ent_free"}} {
		t.Run(fmt.Sprintf("entities=%v", entities), func(t *testing.T) {
			f := newRKGFixture(datasubject.StateVerified)
			f.audit.err = errors.New("audit table gone")
			err := f.run(resolveKGOpts{requestID: "dsr_1", entities: entities})
			if err == nil {
				t.Fatal("an unaudited search ran")
			}
			if f.log.index("candidates") >= 0 || len(f.kg.binds) > 0 {
				t.Fatalf("read or wrote after a failed audit: %v", f.log.events)
			}
		})
	}
}

func TestResolveKG_SearchAuditedBeforeCandidates(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1", names: []string{rkgSentinelNick}}); err != nil {
		t.Fatal(err)
	}
	a, c := f.log.index("audit:"+subjectresolve.ActionSearch), f.log.index("candidates")
	if a < 0 || c < 0 || a > c {
		t.Fatalf("search audit must precede candidates: %v", f.log.events)
	}
	row := f.audit.rows[0]
	if row.Source != "cli" || row.Target != "dsr_1" || row.Principal != "cli:tester" {
		t.Fatalf("search row = %+v", row)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(row.After), &after); err != nil {
		t.Fatal(err)
	}
	if after["subject_id"] != "ds_1" || after["terms"] != float64(2) {
		t.Fatalf("search row After = %v", after)
	}
}

func TestResolveKG_PreviewWithholdsRestricted(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1"}); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.Contains(out, "ent_restricted") || !strings.Contains(out, "details withheld") {
		t.Fatalf("restricted entity not shown as withheld:\n%s", out)
	}
	if strings.Contains(out, "Restricted "+rkgSentinelName) {
		t.Fatalf("restricted entity's name printed:\n%s", out)
	}
	if strings.Contains(out, "--entity ent_restricted") {
		t.Fatalf("restricted entity offered for binding:\n%s", out)
	}
	if !strings.Contains(out, "--entity ent_free") || !strings.Contains(out, "--request dsr_1") {
		t.Fatalf("suggested bind line missing the free entity or the request:\n%s", out)
	}
}

func TestResolveKG_ScreenErrorShowsNoList(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	f.screen.err = errors.New("db down")
	if err := f.run(resolveKGOpts{requestID: "dsr_1"}); err == nil {
		t.Fatal("a failed restriction screen did not fail the preview")
	}
	if strings.Contains(f.out.String(), "ent_free") {
		t.Fatalf("an unscreened list was printed:\n%s", f.out.String())
	}
}

func TestResolveKG_RestrictedEntityIsNeverBound(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	err := f.run(resolveKGOpts{requestID: "dsr_1", entities: []string{"ent_restricted"}})
	if err == nil {
		t.Fatal("binding a restricted entity reported success")
	}
	if len(f.kg.binds) != 0 {
		t.Fatalf("Bind called for a restricted entity: %v", f.kg.binds)
	}
	if n := len(f.audit.rows); n != 2 || f.audit.rows[1].Action != subjectresolve.ActionBindEntity ||
		!strings.Contains(f.audit.rows[1].After, "restriction") {
		t.Fatalf("want search + bind_entity(refused) rows, got %d", n)
	}
}

func TestResolveKG_BindAuditedAfterWrite(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1", entities: []string{"ent_free"}}); err != nil {
		t.Fatal(err)
	}
	b, a := f.log.index("bind:ent_free"), f.log.index("audit:"+subjectresolve.ActionBindEntity)
	if b < 0 || a < 0 || a < b {
		t.Fatalf("bind_entity row must follow the bind: %v", f.log.events)
	}
	if !strings.Contains(f.audit.rows[1].After, `"ok: 3 record(s) linked"`) {
		t.Fatalf("bind row After = %s", f.audit.rows[1].After)
	}
}

func TestResolveKG_DecisionAuditFailureExitsNonZero(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	f.audit.failAfter = 1 // the search row lands; the decision row fails
	err := f.run(resolveKGOpts{requestID: "dsr_1", entities: []string{"ent_free"}})
	if err == nil || !strings.Contains(f.out.String()+err.Error(), "AUDIT WRITE FAILED") {
		t.Fatalf("err = %v, out = %s; want AUDIT WRITE FAILED and a non-zero exit", err, f.out.String())
	}
}

// audit 2026-10-12 (§4.17.8) carried to the CLI: a sentinel name — as the
// display name, a --name, an entity's name, another subject's display name in
// a conflict refusal, and inside a resolver error — reaches no admin_audit row
// and no error the CLI returns (cobra prints it to stderr, which a wrapper may
// journal).
func TestResolveKG_NoPersonalDataInAuditOrErrors(t *testing.T) {
	sentinels := []string{rkgSentinelName, rkgSentinelNick, rkgSentinelOther, strings.ToLower(rkgSentinelName)}
	check := func(t *testing.T, f *rkgFixture, err error) {
		t.Helper()
		for _, row := range f.audit.rows {
			blob, _ := json.Marshal(row)
			for _, s := range sentinels {
				if strings.Contains(string(blob), s) {
					t.Errorf("audit row %s carries personal data %q: %s", row.Action, s, blob)
				}
			}
		}
		if err != nil {
			for _, s := range sentinels {
				if strings.Contains(err.Error(), s) {
					t.Errorf("returned error carries personal data %q: %v", s, err)
				}
			}
		}
	}

	t.Run("preview", func(t *testing.T) {
		f := newRKGFixture(datasubject.StateVerified)
		err := f.run(resolveKGOpts{requestID: "dsr_1", names: []string{rkgSentinelNick}})
		check(t, f, err)
	})
	t.Run("conflict refusal", func(t *testing.T) {
		f := newRKGFixture(datasubject.StateVerified)
		f.kg.bindState = datasubject.KGConflict
		f.kg.bindErr = fmt.Errorf("entity ent_free is held by ds_9 (%q)", rkgSentinelOther)
		err := f.run(resolveKGOpts{requestID: "dsr_1", names: []string{rkgSentinelNick}, entities: []string{"ent_free"}})
		if err == nil {
			t.Fatal("a refused bind exited zero")
		}
		check(t, f, err)
	})
	t.Run("candidate read failure", func(t *testing.T) {
		f := newRKGFixture(datasubject.StateVerified)
		f.kg.candErr = fmt.Errorf("search %q in p1: %w", rkgSentinelNick, errors.New("connection reset"))
		err := f.run(resolveKGOpts{requestID: "dsr_1", names: []string{rkgSentinelNick}})
		if err == nil || !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("err = %v, want the root cause", err)
		}
		check(t, f, err)
	})
}

// Design review r1 finding 2: one search row per invocation, before the first
// read — every Bind re-runs Candidates internally, and that re-check read is
// covered by the single prior row, as one row covers a console POST.
func TestResolveKG_OneSearchRowBeforeFirstBind(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1", entities: []string{"ent_free", "ent_two"}}); err != nil {
		t.Fatal(err)
	}
	searches := 0
	for _, r := range f.audit.rows {
		if r.Action == subjectresolve.ActionSearch {
			searches++
		}
	}
	if searches != 1 {
		t.Fatalf("want exactly one search row, got %d: %v", searches, f.log.events)
	}
	if a, b := f.log.index("audit:"+subjectresolve.ActionSearch), f.log.index("bind:ent_free"); a < 0 || a > b {
		t.Fatalf("search row must precede the first bind: %v", f.log.events)
	}
}

// Design review r1 finding 5: an audit-write failure after a committed bind
// does not stop the loop, and the exit distinguishes "bound, record lost"
// from "refused".
func TestResolveKG_DecisionAuditFailureContinuesAndIsDistinct(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	f.audit.failAfter = 1
	err := f.run(resolveKGOpts{requestID: "dsr_1", entities: []string{"ent_free", "ent_two"}})
	if len(f.kg.binds) != 2 {
		t.Fatalf("loop stopped after an audit failure: binds = %v", f.kg.binds)
	}
	if err == nil || !strings.Contains(err.Error(), "AUDIT WRITE FAILED for ent_free, ent_two") {
		t.Fatalf("err = %v, want both audit-failed entities named", err)
	}
	if strings.Contains(err.Error(), "not bound") {
		t.Fatalf("audit failures reported as refusals: %v", err)
	}
}

// Code review r1 finding 3 (task_20261013050408_087c240dfad66f21): a request
// read failure is reported with context and root cause only, like every other
// CLI read error, not as the repository's raw wrapped error.
func TestResolveKG_RequestReadFailureIsRootCaused(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	f.deps.svc.Requests = failingRequests{}
	err := f.run(resolveKGOpts{requestID: "dsr_1"})
	if err == nil || err.Error() != "read request dsr_1: connection reset" {
		t.Fatalf("err = %v", err)
	}
	if len(f.log.events) != 0 {
		t.Fatalf("ran after a failed request read: %v", f.log.events)
	}
}

type failingRequests struct{}

func (failingRequests) GetRequest(context.Context, string) (datasubject.Request, error) {
	return datasubject.Request{}, fmt.Errorf("scan request %q: %w", rkgSentinelName, errors.New("connection reset"))
}

// Code review r2 minor (task_20261013051010_e562874f1f300320): the page trims
// its project filter; the CLI passed --project raw into the search and the
// audit row.
func TestResolveKG_TrimsProjects(t *testing.T) {
	f := newRKGFixture(datasubject.StateVerified)
	if err := f.run(resolveKGOpts{requestID: "dsr_1", projects: []string{" p1 ", "  "}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.audit.rows[0].After, `"project":"p1"`) {
		t.Fatalf("search row After = %s", f.audit.rows[0].After)
	}
}

// Code review r1 minor: truncateName cut at a byte offset and could split a
// multi-byte rune, printing invalid UTF-8 in the preview table.
func TestTruncateName_RuneSafe(t *testing.T) {
	got := truncateName(strings.Repeat("ž", 60))
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 48 {
		t.Fatalf("truncateName = %q (valid=%v, runes=%d)", got, utf8.ValidString(got), utf8.RuneCountInString(got))
	}
}

// §4.17.9: the coverage hint printed by export/erase/restrict must not
// suggest a command the gate will refuse — an actioned request is no longer
// verified.
func TestWarnIfKGUnresolved_NamesAUsableRequest(t *testing.T) {
	verified := datasubject.Request{ID: "dsr_v", SubjectID: "ds_1", State: datasubject.StateVerified}
	out, _ := captureStdoutFunc(t, func() error { warnIfKGUnresolved(nil, verified); return nil })
	if !strings.Contains(out, "resolve-kg ds_1 --request dsr_v") {
		t.Fatalf("verified hint = %q", out)
	}
	actioned := datasubject.Request{ID: "dsr_a", SubjectID: "ds_1", State: datasubject.StateActioned}
	out, _ = captureStdoutFunc(t, func() error { warnIfKGUnresolved(nil, actioned); return nil })
	if strings.Contains(out, "--request dsr_a") || !strings.Contains(out, "verified request") {
		t.Fatalf("actioned hint = %q", out)
	}
	bound := []datasubject.Identifier{{Kind: datasubject.KindKGEntity}}
	out, _ = captureStdoutFunc(t, func() error { warnIfKGUnresolved(bound, verified); return nil })
	if out != "" {
		t.Fatalf("hint printed although an entity is bound: %q", out)
	}
}

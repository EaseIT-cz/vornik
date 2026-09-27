package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence"
)

// Regulatory record design (2026-09-25): one page, in BOTH editions, for every
// GDPR and AI Act action the product records, organised by obligation. The
// statutory ledgers had no surface at all; a DPO asking "show me every
// action" had nowhere to look (backlog P1 "Regulatory audit surface").

type fakeRegRequests struct {
	all     []datasubject.Request
	live    []datasubject.Request
	reports map[string][2]string // id -> {json, hash}
	reads   int
}

func (f *fakeRegRequests) ListRequests(_ context.Context, limit, offset int) ([]datasubject.Request, error) {
	if offset >= len(f.all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(f.all) {
		end = len(f.all)
	}
	return f.all[offset:end], nil
}
func (f *fakeRegRequests) ListLiveRequests(context.Context) ([]datasubject.Request, error) {
	return f.live, nil
}
func (f *fakeRegRequests) GetRequest(_ context.Context, id string) (datasubject.Request, error) {
	for _, r := range f.all {
		if r.ID == id {
			return r, nil
		}
	}
	return datasubject.Request{}, persistence.ErrNotFound
}
func (f *fakeRegRequests) GetRequestReport(_ context.Context, id string) (string, string, error) {
	f.reads++
	for _, r := range f.all {
		if r.ID == id {
			if rep, ok := f.reports[id]; ok {
				return rep[0], rep[1], nil
			}
			return "", "", persistence.ErrReportNotRetained
		}
	}
	return "", "", persistence.ErrNotFound
}

type fakeRegIncidents struct{ all, live []incident.Incident }

func (f *fakeRegIncidents) List(_ context.Context, limit, offset int) ([]incident.Incident, error) {
	if offset >= len(f.all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(f.all) {
		end = len(f.all)
	}
	return f.all[offset:end], nil
}
func (f *fakeRegIncidents) ListLive(context.Context) ([]incident.Incident, error) { return f.live, nil }

type fakeRegDisclosures struct {
	rows     []persistence.ChannelDisclosureSummary
	from, to time.Time
}

func (f *fakeRegDisclosures) SummaryBetween(_ context.Context, from, to time.Time) ([]persistence.ChannelDisclosureSummary, error) {
	f.from, f.to = from, to
	return f.rows, nil
}

type failingAudit struct{ stubAdminAuditRepo }

func (*failingAudit) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return errors.New("audit sink down")
}

const (
	regSubjectEmail = "ada.lovelace@example.org"
	regIncidentText = "Laptop of J. Doe stolen from car"
)

func regFixture(now time.Time) (*fakeRegRequests, *fakeRegIncidents, *fakeRegDisclosures) {
	overdue := datasubject.Request{ID: "dsr-overdue", SubjectID: "subj_1", Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: now.Add(-40 * 24 * time.Hour)}
	closed := datasubject.Request{ID: "dsr-closed-late", SubjectID: "subj_2", Kind: datasubject.RequestErasure,
		State: datasubject.StateClosed, OpenedAt: now.Add(-60 * 24 * time.Hour), ReportHash: "abcdef0123456789abcdef",
		ReportRetained: true}
	reqs := &fakeRegRequests{all: []datasubject.Request{overdue, closed}, live: []datasubject.Request{overdue},
		reports: map[string][2]string{"dsr-closed-late": {`{"erased":[]}`, "abcdef0123456789abcdef"}}}
	soon := incident.Incident{ID: "inc-soon", State: incident.StateDetected, BecameAwareAt: now.Add(-60 * time.Hour),
		Facts: regIncidentText, Effects: "", Remedial: ""}
	incs := &fakeRegIncidents{all: []incident.Incident{soon}, live: []incident.Incident{soon}}
	disc := &fakeRegDisclosures{rows: []persistence.ChannelDisclosureSummary{
		{Channel: "slack", TextHash: "1234567890abcdef1234", Count: 7, FirstServed: now.Add(-9 * 24 * time.Hour), LastServed: now},
	}}
	return reqs, incs, disc
}

func regServer(reqs RegulatoryRequests, incs RegulatoryIncidents, disc RegulatoryDisclosures, audit persistence.AdminAuditRepository) *Server {
	return NewServer(
		WithRegulatoryLedgers(reqs, incs, disc, audit),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
	)
}

func regGet(s *Server, path string, withReq func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.operatorRegulatoryRouter(rec, withReq(httptest.NewRequest(http.MethodGet, path, nil)))
	return rec
}

func asKey(key string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(api.ContextWithAPIKeyForTesting(api.ContextWithAuthEnabled(r.Context(), true), key))
	}
}

// The gate, in a Community-shaped server (no Enterprise admin options): a
// non-operator gets 403 — NOT 501, the admin gate's Community answer, which
// would silently re-gate the page out of CE (review F7).
func TestRegulatory_GateIsTheCEOperatorCapability(t *testing.T) {
	reqs, incs, disc := regFixture(time.Now())
	s := regServer(reqs, incs, disc, &stubAdminAuditRepo{})
	for _, path := range []string{"/operator/regulatory", "/operator/regulatory/requests/dsr-closed-late/report"} {
		if rec := regGet(s, path, asKey("sk-plain")); rec.Code != http.StatusForbidden {
			t.Fatalf("%s non-operator: want 403 (not 501), got %d", path, rec.Code)
		}
	}
	if rec := regGet(s, "/operator/regulatory", asKey("sk-operator")); rec.Code != http.StatusOK {
		t.Fatalf("operator: want 200, got %d", rec.Code)
	}
	if rec := regGet(s, "/operator/regulatory", withAuthOff); rec.Code != http.StatusOK {
		t.Fatalf("auth off: want 200, got %d", rec.Code)
	}
}

// The clocks come first, judged by the ledgers' own methods; ids, grounds,
// dispositions and hashes are shown; personal data never is.
func TestRegulatory_PageShowsTheClocksAndNoPersonalData(t *testing.T) {
	reqs, incs, disc := regFixture(time.Now())
	s := regServer(reqs, incs, disc, &stubAdminAuditRepo{})
	body := regGet(s, "/operator/regulatory", withAuthOff).Body.String()
	cut := strings.Index(body, "id=\"requests\"")
	if cut < 0 {
		t.Fatalf("the requests section is missing:\n%s", body)
	}
	attention := body[:cut]
	for _, want := range []string{"dsr-overdue", "inc-soon"} {
		if !strings.Contains(attention, want) {
			t.Errorf("needs-attention must list %s", want)
		}
	}
	if strings.Contains(attention, "dsr-closed-late") {
		t.Error("a closed request past its deadline is not 'needs attention'")
	}
	for _, want := range []string{"dsr-closed-late", "subj_2", "Art 17", "abcdef012345", "slack", "7"} {
		if !strings.Contains(body, want) {
			t.Errorf("the record must show %q", want)
		}
	}
	if !strings.Contains(body, "/ui/operator/regulatory/requests/dsr-closed-late/report") {
		t.Error("a request with a retained report must link its download")
	}
	if strings.Contains(body, "/ui/operator/regulatory/requests/dsr-overdue/report") {
		t.Error("a request without a retained report must not link a download")
	}
	for _, leak := range []string{regSubjectEmail, regIncidentText, "J. Doe"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page leaked personal data %q", leak)
		}
	}
}

// On SQLite the ledgers do not exist: say so, never render an empty table
// that reads as "no requests" (design §5, F8).
func TestRegulatory_SQLiteSaysNotRecorded(t *testing.T) {
	_, _, disc := regFixture(time.Now())
	s := regServer(nil, nil, disc, &stubAdminAuditRepo{})
	body := regGet(s, "/operator/regulatory", withAuthOff).Body.String()
	if strings.Count(body, "Not recorded on this deployment") < 2 {
		t.Fatalf("both statutory sections must say they are not recorded:\n%s", body)
	}
	rec := regGet(s, "/operator/regulatory/requests/x/report", withAuthOff)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not recorded") {
		t.Fatalf("SQLite download: want 404 'not recorded', got %d %q", rec.Code, rec.Body.String())
	}
}

// Disclosures: aggregated, a bounded range, no session ids.
func TestRegulatory_DisclosureRangeIsBounded(t *testing.T) {
	reqs, incs, disc := regFixture(time.Now())
	s := regServer(reqs, incs, disc, &stubAdminAuditRepo{})
	regGet(s, "/operator/regulatory", withAuthOff)
	if got := disc.to.Sub(disc.from); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("default window = %v, want 30 days", got)
	}
	body := regGet(s, "/operator/regulatory?days=400", withAuthOff).Body.String()
	if !strings.Contains(body, "366 days") {
		t.Error("a range wider than 366 days must be refused with a message")
	}
	// The boundary (review): 366 is accepted, 367 is refused.
	regGet(s, "/operator/regulatory?days=366", withAuthOff)
	if got := disc.to.Sub(disc.from); got != 366*24*time.Hour {
		t.Errorf("days=366 must be accepted, window = %v", got)
	}
	if !strings.Contains(regGet(s, "/operator/regulatory?days=367", withAuthOff).Body.String(), "366 days") {
		t.Error("days=367 must be refused")
	}
}

// The download: audited BEFORE the body; refused when the audit cannot be
// written (no attachment header, no body); refused without an audit sink,
// before anything is read; two distinguishable 404s.
func TestRegulatory_ReportDownloadIsAuditedAndFailsClosed(t *testing.T) {
	reqs, incs, disc := regFixture(time.Now())
	audit := &stubAdminAuditRepo{}
	s := regServer(reqs, incs, disc, audit)
	rec := regGet(s, "/operator/regulatory/requests/dsr-closed-late/report", withAuthOff)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") ||
		rec.Header().Get("Cache-Control") != "no-store" || rec.Body.String() != `{"erased":[]}` {
		t.Fatalf("download: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if len(audit.rows) != 1 || audit.rows[0].Target != "dsr-closed-late" || !strings.Contains(audit.rows[0].After, "abcdef0123456789abcdef") {
		t.Fatalf("the download must write an admin_audit row naming the request and hash: %+v", audit.rows)
	}

	s = regServer(reqs, incs, disc, &failingAudit{})
	rec = regGet(s, "/operator/regulatory/requests/dsr-closed-late/report", withAuthOff)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Disposition") != "" || strings.Contains(rec.Body.String(), "erased") {
		t.Fatalf("audit failure must refuse with no attachment and no report: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}

	reads := reqs.reads
	s = regServer(reqs, incs, disc, nil)
	rec = regGet(s, "/operator/regulatory/requests/dsr-closed-late/report", withAuthOff)
	if rec.Code == http.StatusOK || reqs.reads != reads {
		t.Fatalf("no audit sink: must refuse before reading (code %d, reads %d->%d)", rec.Code, reads, reqs.reads)
	}
	if strings.Contains(regGet(s, "/operator/regulatory", withAuthOff).Body.String(), "/report\"") {
		t.Error("no audit sink: the download link must not be rendered")
	}

	s = regServer(reqs, incs, disc, &stubAdminAuditRepo{})
	if rec := regGet(s, "/operator/regulatory/requests/dsr-overdue/report", withAuthOff); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no retained report") {
		t.Fatalf("no report: %d %q", rec.Code, rec.Body.String())
	}
	if rec := regGet(s, "/operator/regulatory/requests/nope/report", withAuthOff); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such request") {
		t.Fatalf("no row: %d %q", rec.Code, rec.Body.String())
	}
}

// Art 15/20 exports are not retained (operator decision 2026-09-25, design
// §4.1): the export IS the subject's personal data. The page must say so, per
// right, instead of a blank cell that reads as a missing report — and the 404
// must stop saying "only erasure retains its report today", which read as a
// gap still to be filled.
func TestRegulatory_ExportsSayTheyAreNotRetained(t *testing.T) {
	now := time.Now()
	reqs, incs, disc := regFixture(now)
	export := datasubject.Request{ID: "dsr-export", SubjectID: "subj_3", Kind: datasubject.RequestPortability,
		State: datasubject.StateClosed, OpenedAt: now.Add(-5 * 24 * time.Hour), ReportHash: "feedface0123456789"}
	legacy := datasubject.Request{ID: "dsr-legacy-erasure", SubjectID: "subj_4", Kind: datasubject.RequestErasure,
		State: datasubject.StateClosed, OpenedAt: now.Add(-90 * 24 * time.Hour), ReportHash: "0badc0de01234567"}
	reqs.all = append(reqs.all, export, legacy)
	s := regServer(reqs, incs, disc, &stubAdminAuditRepo{})

	body := regGet(s, "/operator/regulatory", withAuthOff).Body.String()
	if !strings.Contains(body, "export is the subject&#39;s personal data") {
		t.Errorf("an export row must say why it is not retained:\n%s", body)
	}
	if strings.Count(body, "not retained") < 2 {
		t.Error("both unretained rows (export and legacy erasure) must say 'not retained'")
	}
	if strings.Count(body, "the export is the subject") != 1 {
		t.Error("only the export row may give the export reason, not the legacy erasure row")
	}
	if strings.Contains(body, "/requests/dsr-export/report") {
		t.Error("an unretained export must not link a download")
	}

	rec := regGet(s, "/operator/regulatory/requests/dsr-export/report", withAuthOff)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "personal data") ||
		strings.Contains(rec.Body.String(), "today") {
		t.Fatalf("export 404 must give the reason, not a 'today' gap: %d %q", rec.Code, rec.Body.String())
	}
	rec = regGet(s, "/operator/regulatory/requests/dsr-legacy-erasure/report", withAuthOff)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not retained") ||
		strings.Contains(rec.Body.String(), "export") {
		t.Fatalf("legacy erasure 404 must say not retained and never call it an export (review F2): %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "design") {
		t.Error("an HTTP body must not cite an internal design section (review F3)")
	}
}

// The profile-use audit's SQLite gap is named only where it applies: on a
// Postgres host the unconditional "Not recorded on SQLite" read as a fault
// (seen on the first deploy, 2026-09-25).
func TestRegulatory_ProfileAuditSQLiteNoteOnlyOnSQLite(t *testing.T) {
	reqs, incs, disc := regFixture(time.Now())
	if body := regGet(regServer(reqs, incs, disc, &stubAdminAuditRepo{}), "/operator/regulatory", withAuthOff).Body.String(); strings.Contains(body, "Not recorded on SQLite") {
		t.Error("a Postgres-backed page must not print the SQLite gap")
	}
	if body := regGet(regServer(nil, nil, disc, &stubAdminAuditRepo{}), "/operator/regulatory", withAuthOff).Body.String(); !strings.Contains(body, "not recorded on the SQLite backend") {
		t.Error("a SQLite-backed page must name the profile-use audit's gap")
	}
}

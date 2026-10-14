package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #345 (2026-10-12): knowledge-graph resolution was CLI-only and the
// §5.1 free-text backstop did not exist, so an operator working a request in
// /ui had no way to widen coverage and a person the extractor never turned
// into an entity was unreachable. These tests pin the console page that closes
// both, and the bounds that stop it becoming a bulk export.
//
// see LLD § https://docs.vornik.io §4.17

type fakeResolveKG struct {
	cands   []datasubject.KGCandidate
	binds   []datasubject.KGBindRequest
	candErr error
	bindErr error
}

func (f *fakeResolveKG) Candidates(_ context.Context, _ string, _, _ []string) ([]datasubject.KGCandidate, error) {
	if f.candErr != nil {
		return nil, f.candErr
	}
	return f.cands, nil
}

func (f *fakeResolveKG) Bind(_ context.Context, req datasubject.KGBindRequest) (datasubject.KGBindResult, error) {
	f.binds = append(f.binds, req)
	if f.bindErr != nil {
		return datasubject.KGBindResult{EntityID: req.EntityID}, f.bindErr
	}
	for _, c := range f.cands {
		if c.Entity.ID == req.EntityID && c.State == datasubject.KGConflict {
			return datasubject.KGBindResult{EntityID: req.EntityID, State: c.State}, errors.New("conflict refused")
		}
	}
	return datasubject.KGBindResult{EntityID: req.EntityID, LinksAdded: 3}, nil
}

type fakeResolveLinks struct {
	subject  datasubject.Subject
	ids      []datasubject.Identifier
	links    []datasubject.Link
	added    []datasubject.Link
	unlinked []string
}

func (f *fakeResolveLinks) GetSubject(context.Context, string) (datasubject.Subject, error) {
	return f.subject, nil
}
func (f *fakeResolveLinks) ListIdentifiers(context.Context, string) ([]datasubject.Identifier, error) {
	return f.ids, nil
}
func (f *fakeResolveLinks) ListLinks(context.Context, string) ([]datasubject.Link, error) {
	return f.links, nil
}
func (f *fakeResolveLinks) AddLinkIfAbsent(_ context.Context, _ string, l datasubject.Link) (bool, error) {
	if err := l.Validate(); err != nil {
		return false, err
	}
	f.added = append(f.added, l)
	return true, nil
}
func (f *fakeResolveLinks) DeleteOperatorLink(_ context.Context, _, _, row string) (bool, error) {
	f.unlinked = append(f.unlinked, row)
	return true, nil
}

type fakeTextSearch struct {
	rows    []persistence.SubjectTextMention
	queries []persistence.SubjectTextQuery
	onCall  func()
}

func (f *fakeTextSearch) FindMentions(_ context.Context, q persistence.SubjectTextQuery) ([]persistence.SubjectTextMention, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.queries = append(f.queries, q)
	var out []persistence.SubjectTextMention
	for _, m := range f.rows {
		if q.ChunkID != "" && m.ChunkID != q.ChunkID {
			continue
		}
		if !strings.Contains(strings.ToLower(m.Content), strings.ToLower(q.Term)) {
			continue
		}
		out = append(out, m)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

type fakeScreen struct {
	entities map[string]bool
	err      error
}

func (f *fakeScreen) RestrictedEntities(_ context.Context, ids []string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, id := range ids {
		if f.entities[id] {
			out[id] = true
		}
	}
	return out, nil
}
func (f *fakeScreen) RestrictedChunks(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, f.err
}

const (
	resolveReq      = "dsr-verified"
	resolveSubject  = "ds_jane"
	resolveFarText  = "SECRET-UNRELATED-PAYLOAD"
	resolveNickname = "Janey"
)

type resolveFixture struct {
	reqs   *fakeRegRequests
	kg     *fakeResolveKG
	links  *fakeResolveLinks
	text   *fakeTextSearch
	screen *fakeScreen
	audit  *stubAdminAuditRepo
}

func newResolveFixture() *resolveFixture {
	now := time.Now()
	verified := datasubject.Request{ID: resolveReq, SubjectID: resolveSubject, Kind: datasubject.RequestErasure,
		State: datasubject.StateVerified, OpenedAt: now.Add(-24 * time.Hour), ErasureGround: "art17_1_a"}
	open := datasubject.Request{ID: "dsr-open", SubjectID: resolveSubject, Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: now}
	closed := datasubject.Request{ID: "dsr-closed", SubjectID: resolveSubject, Kind: datasubject.RequestAccess,
		State: datasubject.StateClosed, OpenedAt: now}
	far := strings.Repeat("x", 200) + resolveFarText
	return &resolveFixture{
		reqs: &fakeRegRequests{all: []datasubject.Request{verified, open, closed}},
		kg: &fakeResolveKG{cands: []datasubject.KGCandidate{
			{Entity: datasubject.KGEntity{ID: "ent_free", ProjectID: "p1", CanonicalName: "Jane Doe"}, MatchedOn: "Jane Doe", MentionCount: 4, State: datasubject.KGUnbound},
			{Entity: datasubject.KGEntity{ID: "ent_held", ProjectID: "p1", CanonicalName: "Jane D. Other"}, MatchedOn: "Jane", State: datasubject.KGConflict, BoundSubjectID: "ds_other"},
			{Entity: datasubject.KGEntity{ID: "ent_restricted", ProjectID: "p1", CanonicalName: "Restricted Person Name"}, MatchedOn: "Jane", State: datasubject.KGConflict, BoundSubjectID: "ds_r"},
		}},
		links: &fakeResolveLinks{subject: datasubject.Subject{ID: resolveSubject, DisplayName: "Jane Doe"}},
		text: &fakeTextSearch{rows: []persistence.SubjectTextMention{
			{ChunkID: "chunk_new", ProjectID: "p1", Content: "Called Jane Doe about the invoice." + far, CreatedAt: "2026-10-01"},
			{ChunkID: "chunk_nick", ProjectID: "p1", Content: "lunch with " + resolveNickname + " today", CreatedAt: "2026-10-02"},
			{ChunkID: "chunk_mine", ProjectID: "p1", Content: "Jane Doe signed", LinkedHereSource: "operator_asserted"},
			{ChunkID: "chunk_kg", ProjectID: "p1", Content: "Jane Doe again", LinkedHereSource: "kg_extraction", OtherSubjects: 2},
		}},
		screen: &fakeScreen{entities: map[string]bool{"ent_restricted": true}},
		audit:  &stubAdminAuditRepo{},
	}
}

func (f *resolveFixture) server() *Server {
	return NewServer(
		WithRegulatoryLedgers(f.reqs, nil, nil, f.audit),
		WithSubjectResolution(f.kg, f.links, f.text, f.screen),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
	)
}

func resolvePath(id string) string { return "/operator/regulatory/requests/" + id + "/resolve" }

func resolveDo(s *Server, method, path string, form url.Values, as func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	s.operatorRegulatoryRouter(rec, as(req))
	return rec
}

func searchForm(names ...string) url.Values {
	v := url.Values{"action": {"search"}}
	for _, n := range names {
		v.Add("name", n)
	}
	return v
}

func (f *resolveFixture) actions(prefix string) []persistence.AdminAuditEntry {
	var out []persistence.AdminAuditEntry
	for _, r := range f.audit.rows {
		if strings.HasPrefix(r.Action, prefix) {
			out = append(out, *r)
		}
	}
	return out
}

// The page is admin-class: a non-operator key is refused on GET and POST, and
// nothing is searched or written.
func TestSubjectResolve_NonOperatorRefused(t *testing.T) {
	f := newResolveFixture()
	s := f.server()
	if rec := resolveDo(s, http.MethodGet, resolvePath(resolveReq), nil, asKey("sk-plain")); rec.Code != http.StatusForbidden {
		t.Fatalf("GET non-operator: want 403, got %d", rec.Code)
	}
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(), asKey("sk-plain")); rec.Code != http.StatusForbidden {
		t.Fatalf("POST non-operator: want 403, got %d", rec.Code)
	}
	if len(f.text.queries) != 0 || len(f.audit.rows) != 0 {
		t.Fatal("a refused caller must not search or write audit rows")
	}
	if rec := resolveDo(s, http.MethodGet, resolvePath(resolveReq), nil, asKey("sk-operator")); rec.Code != http.StatusOK {
		t.Fatalf("operator GET: want 200, got %d", rec.Code)
	}
}

// Purpose limitation: only a VERIFIED request may drive a name search.
func TestSubjectResolve_RefusesUnverifiedOrClosedRequest(t *testing.T) {
	f := newResolveFixture()
	s := f.server()
	for _, id := range []string{"dsr-open", "dsr-closed"} {
		if rec := resolveDo(s, http.MethodPost, resolvePath(id), searchForm(), withAuthOff); rec.Code != http.StatusConflict {
			t.Errorf("%s: want 409, got %d", id, rec.Code)
		}
	}
	if rec := resolveDo(s, http.MethodPost, resolvePath("dsr-missing"), searchForm(), withAuthOff); rec.Code != http.StatusNotFound {
		t.Errorf("unknown request: want 404, got %d", rec.Code)
	}
	if len(f.text.queries) != 0 {
		t.Fatal("an unverified request must never reach the search")
	}
}

// On SQLite the resolver is not wired (the KG subject index is Postgres-only,
// GDPR design §4.19.4): the page says so and a POST does nothing.
func TestSubjectResolve_SQLiteSaysNotRecorded(t *testing.T) {
	s := NewServer(WithRegulatoryLedgers(nil, nil, nil, &stubAdminAuditRepo{}))
	rec := resolveDo(s, http.MethodGet, resolvePath(resolveReq), nil, withAuthOff)
	if !strings.Contains(rec.Body.String(), "Not available on this deployment") ||
		strings.Contains(rec.Body.String(), "no subject-request ledger") {
		t.Fatalf("SQLite GET must say resolution is not available (and not deny the ledger), got %d:\n%s", rec.Code, rec.Body.String())
	}
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff); rec.Code != http.StatusNotFound {
		t.Fatalf("SQLite POST: want 404, got %d", rec.Code)
	}
}

// The search is audited BEFORE it reads, with term lengths and never the terms;
// a failing audit refuses the search.
func TestSubjectResolve_SearchAuditedFirstWithHashesOnly(t *testing.T) {
	f := newResolveFixture()
	audited := false
	f.text.onCall = func() {
		if len(f.actions("subject.resolve.search")) > 0 {
			audited = true
		}
	}
	s := f.server()
	rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(resolveNickname), withAuthOff)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d:\n%s", rec.Code, rec.Body.String())
	}
	if !audited {
		t.Fatal("the search ran before its audit row was written")
	}
	rows := f.actions("subject.resolve.search")
	if len(rows) != 1 || rows[0].Target != resolveReq {
		t.Fatalf("want one search audit row targeting the request, got %+v", rows)
	}
	if strings.Contains(rows[0].After, resolveNickname) || strings.Contains(rows[0].After, "Jane Doe") {
		t.Errorf("the audit row must not carry the names: %s", rows[0].After)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(rows[0].After), &after); err != nil || after["subject_id"] != resolveSubject {
		t.Errorf("audit After must be JSON naming the subject: %s (%v)", rows[0].After, err)
	}
	if lens, _ := after["term_lengths"].([]any); len(lens) != 2 || lens[0] != float64(8) || lens[1] != float64(5) {
		t.Errorf("want one length per term (display name 8, nickname 5), got %v", after["term_lengths"])
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("a page showing personal data must be no-store and no-referrer")
	}

	failing := newResolveFixture()
	s2 := NewServer(
		WithRegulatoryLedgers(failing.reqs, nil, nil, &failingAudit{}),
		WithSubjectResolution(failing.kg, failing.links, failing.text, failing.screen),
	)
	if rec := resolveDo(s2, http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing audit: want 500, got %d", rec.Code)
	}
	if len(failing.text.queries) != 0 {
		t.Fatal("an unaudited search must not run")
	}
}

// Review round 1 F2: a decision POST is a search first — with a failing audit
// it performs no re-check read and no write.
func TestSubjectResolve_DecisionRefusedWhenAuditFails(t *testing.T) {
	f := newResolveFixture()
	s := NewServer(
		WithRegulatoryLedgers(f.reqs, nil, nil, &failingAudit{}),
		WithSubjectResolution(f.kg, f.links, f.text, f.screen),
	)
	form := searchForm()
	form.Set("action", "apply")
	form.Set("chunk:chunk_new", "confirm")
	form.Set("chunk:chunk_mine", "unlink")
	form.Set("kg:ent_free", "confirm")
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), form, withAuthOff); rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if len(f.text.queries) != 0 || len(f.links.added) != 0 || len(f.links.unlinked) != 0 || len(f.kg.binds) != 0 {
		t.Fatalf("an unaudited decision POST read or wrote: queries=%d added=%d unlinked=%d binds=%d",
			len(f.text.queries), len(f.links.added), len(f.links.unlinked), len(f.kg.binds))
	}
}

// Results: bounded snippets only, restricted entities withheld, binder links
// shown without an action, and the coverage signal.
func TestSubjectResolve_RendersBoundedCandidates(t *testing.T) {
	f := newResolveFixture()
	body := resolveDo(f.server(), http.MethodPost, resolvePath(resolveReq), searchForm(resolveNickname), withAuthOff).Body.String()
	for _, want := range []string{"chunk_new", "chunk_nick", "Jane Doe", "ent_free", "held under an active restriction", "kg_extraction", "No knowledge-graph entity is bound"} {
		if !strings.Contains(body, want) {
			t.Errorf("page must show %q", want)
		}
	}
	if strings.Contains(body, resolveFarText) {
		t.Error("text 200 characters from the match leaked: snippets must be bounded")
	}
	if strings.Contains(body, "Restricted Person Name") {
		t.Error("a restricted entity's name was rendered")
	}
	if strings.Contains(body, `name="kg:ent_restricted"`) || strings.Contains(body, `name="kg:ent_held"`) {
		t.Error("a restricted or conflicting entity must offer no action")
	}
	if strings.Contains(body, `name="chunk:chunk_kg"`) {
		t.Error("a binder-derived link must offer no action")
	}
	if !strings.Contains(body, `name="chunk:chunk_mine" value="unlink"`) {
		t.Error("an operator-asserted link must offer unlink")
	}
	if !strings.Contains(body, "border-control") {
		t.Error("form controls must use the WCAG control-border token (#164)")
	}
}

// A screen failure withholds the KG list rather than showing it unscreened.
func TestSubjectResolve_ScreenErrorWithholdsKG(t *testing.T) {
	f := newResolveFixture()
	f.screen.err = errors.New("db down")
	body := resolveDo(f.server(), http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff).Body.String()
	if strings.Contains(body, "ent_free") {
		t.Fatal("the KG list was shown although the restriction screen failed")
	}
	if !strings.Contains(body, "withheld") {
		t.Error("the page must say the KG candidates were withheld")
	}
}

// Term bounds: too short, too long, too many → 400 and nothing searched.
func TestSubjectResolve_TermBounds(t *testing.T) {
	f := newResolveFixture()
	s := f.server()
	for name, form := range map[string]url.Values{
		"short":    searchForm("ab"),
		"long":     searchForm(strings.Repeat("a", 101)),
		"too many": searchForm("aaa", "bbb", "ccc", "ddd", "eee", "fff"),
	} {
		if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), form, withAuthOff); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, rec.Code)
		}
	}
	if len(f.text.queries) != 0 {
		t.Fatal("an out-of-bounds search must not run")
	}
}

// The result cap: at most 50 rows, and the page says more exist.
func TestSubjectResolve_ResultCap(t *testing.T) {
	f := newResolveFixture()
	f.text.rows = nil
	for i := 0; i < 70; i++ {
		f.text.rows = append(f.text.rows, persistence.SubjectTextMention{ChunkID: "c" + strings.Repeat("0", 3) + string(rune('A'+i%26)) + string(rune('a'+i/26)), ProjectID: "p", Content: "Jane Doe"})
	}
	body := resolveDo(f.server(), http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff).Body.String()
	if n := strings.Count(body, `class="text-candidate `); n != 50 {
		t.Errorf("want 50 rendered candidates, got %d", n)
	}
	if !strings.Contains(body, "More matches exist") {
		t.Error("a capped search must say more matches exist")
	}
}

// Code review 2026-10-12 (finding 1): the KG list was uncapped at the handler
// (the resolver allows thousands per name and project), so a broad name could
// render every matching person's name and aliases. Capped like the text list.
func TestSubjectResolve_KGCap(t *testing.T) {
	f := newResolveFixture()
	f.kg.cands = nil
	for i := 0; i < 70; i++ {
		id := "ent_" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		f.kg.cands = append(f.kg.cands, datasubject.KGCandidate{
			Entity: datasubject.KGEntity{ID: id, ProjectID: "p", CanonicalName: "Jane " + id}, State: datasubject.KGUnbound})
	}
	body := resolveDo(f.server(), http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff).Body.String()
	if n := strings.Count(body, `class="kg-candidate `); n != 50 {
		t.Errorf("want 50 rendered KG candidates, got %d", n)
	}
	if !strings.Contains(body, "More entities match") {
		t.Error("a capped KG list must say more entities match")
	}
}

// The rate limit: the 21st search within the window is refused.
func TestSubjectResolve_RateLimited(t *testing.T) {
	f := newResolveFixture()
	s := f.server()
	for i := 0; i < 20; i++ {
		if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff); rec.Code != http.StatusOK {
			t.Fatalf("search %d: want 200, got %d", i+1, rec.Code)
		}
	}
	before := len(f.text.queries)
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(), withAuthOff); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("21st search: want 429, got %d", rec.Code)
	}
	if len(f.text.queries) != before {
		t.Fatal("a rate-limited search must not run")
	}
}

// Decisions: confirm re-checks and links at operator_asserted/probable/unknown;
// reject writes an audit row only; unlink removes only the operator's link;
// KG confirm goes through Bind; a restricted entity is refused.
func TestSubjectResolve_Decisions(t *testing.T) {
	f := newResolveFixture()
	form := searchForm(resolveNickname)
	form.Set("action", "apply")
	form.Set("chunk:chunk_new", "confirm")
	form.Set("chunk:chunk_nick", "reject")
	form.Set("chunk:chunk_mine", "unlink")
	form.Set("chunk:chunk_kg", "confirm")   // already linked by a binder: refused
	form.Set("chunk:chunk_gone", "confirm") // no longer matches: refused
	form.Set("kg:ent_free", "confirm")
	form.Set("kg:ent_restricted", "confirm") // crafted: refused by the screen
	rec := resolveDo(f.server(), http.MethodPost, resolvePath(resolveReq), form, withAuthOff)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: want 200, got %d:\n%s", rec.Code, rec.Body.String())
	}
	if len(f.links.added) != 1 {
		t.Fatalf("want exactly one link added, got %+v", f.links.added)
	}
	l := f.links.added[0]
	if l.RowID != "chunk_new" || l.Table != datasubject.TableProjectMemoryChunks || l.ProjectID != "p1" ||
		l.Source != datasubject.SourceOperatorAsserted || l.Confidence != datasubject.ConfidenceProbable ||
		l.Exclusivity != datasubject.UnknownExclusivity {
		t.Errorf("confirmed link has the wrong shape: %+v", l)
	}
	if len(f.links.unlinked) != 1 || f.links.unlinked[0] != "chunk_mine" {
		t.Errorf("want chunk_mine unlinked, got %v", f.links.unlinked)
	}
	if len(f.kg.binds) != 1 || f.kg.binds[0].EntityID != "ent_free" || f.kg.binds[0].SubjectID != resolveSubject {
		t.Errorf("want one Bind of ent_free, got %+v", f.kg.binds)
	}
	outcomes := map[string]string{}
	for _, r := range f.audit.rows {
		if r.Action == "subject.resolve.search" {
			continue
		}
		var after map[string]any
		_ = json.Unmarshal([]byte(r.After), &after)
		id, _ := after["id"].(string)
		out, _ := after["outcome"].(string)
		outcomes[r.Action+" "+id] = out
	}
	for key, want := range map[string]string{
		"subject.resolve.link_chunk chunk_new":       "ok",
		"subject.resolve.reject chunk_nick":          "ok",
		"subject.resolve.unlink_chunk chunk_mine":    "ok",
		"subject.resolve.bind_entity ent_free":       "ok",
		"subject.resolve.link_chunk chunk_kg":        "refused",
		"subject.resolve.link_chunk chunk_gone":      "refused",
		"subject.resolve.bind_entity ent_restricted": "refused",
	} {
		if got := outcomes[key]; !strings.HasPrefix(got, want) {
			t.Errorf("audit %s: outcome %q, want prefix %q (all: %v)", key, got, want, outcomes)
		}
	}
}

// The regulatory record links the resolution page from VERIFIED requests only,
// and only when the page is wired (the first gap of #345: no signal in /ui).
func TestRegulatory_LinksResolveForVerifiedRequestsOnly(t *testing.T) {
	f := newResolveFixture()
	body := regGet(f.server(), "/operator/regulatory", withAuthOff).Body.String()
	if !strings.Contains(body, "/ui/operator/regulatory/requests/"+resolveReq+"/resolve") {
		t.Error("a verified request must link the resolution page")
	}
	for _, id := range []string{"dsr-open", "dsr-closed"} {
		if strings.Contains(body, "/ui/operator/regulatory/requests/"+id+"/resolve") {
			t.Errorf("%s is not verified and must not link the resolution page", id)
		}
	}
	unwired := regGet(regServer(f.reqs, nil, nil, f.audit), "/operator/regulatory", withAuthOff).Body.String()
	if strings.Contains(unwired, "/resolve") {
		t.Error("the link must not appear when the resolution page is not wired")
	}
}

// A snippet is cut around the match; when the term cannot be located it is
// withheld, never replaced by the start of the record.
func TestSubjectResolve_Snippet(t *testing.T) {
	pre, match, post, ok := snippet(strings.Repeat("a", 100)+"Jane DOE"+strings.Repeat("b", 100), []string{"jane doe"}, 10)
	if !ok || match != "Jane DOE" || pre != "…"+strings.Repeat("a", 10) || post != strings.Repeat("b", 10)+"…" {
		t.Errorf("snippet = %q|%q|%q ok=%v", pre, match, post, ok)
	}
	if _, _, _, ok := snippet("nothing here", []string{"jane"}, 10); ok {
		t.Error("an unlocatable match must withhold the snippet")
	}
	if row := textRow(persistence.SubjectTextMention{ChunkID: "c", Content: "nothing here"}, []string{"jane"}); !row.SnippetWithheld || row.CanConfirm {
		t.Error("a candidate whose snippet is withheld must not be confirmable")
	}
	if l := newSearchLimiter(2, time.Minute); !l.allow("a", time.Unix(0, 0)) || !l.allow("a", time.Unix(1, 0)) ||
		l.allow("a", time.Unix(2, 0)) || !l.allow("a", time.Unix(61, 0)) || !l.allow("b", time.Unix(2, 0)) {
		t.Error("sliding window: two per minute per key")
	}
}

// Security review 2026-10-12 (sensitive-data-to-audit-log): the audit rows and
// logs of this page must say WHO did WHAT — never the personal data. Three real
// leaks: the search row carried UNKEYED sha256 prefixes of the names (a name is
// low-entropy, so a dictionary reverses it); a refused KG bind recorded Bind's
// error text, which quotes the other subject's display name; and a failed
// candidate search logged the resolver's error, which quotes the searched name.
func TestSubjectResolve_NoPersonalDataInAuditOrLogs(t *testing.T) {
	const pii = "Zebulon Quixtrot-Sentinel"
	f := newResolveFixture()
	f.kg.bindErr = errors.New(`entity ent_free is already bound to subject ds_other ("` + pii + `"): refused`)
	var logs strings.Builder
	s := NewServer(
		WithRegulatoryLedgers(f.reqs, nil, nil, f.audit),
		WithSubjectResolution(f.kg, f.links, f.text, f.screen),
		WithLogger(zerolog.New(&logs)),
	)
	form := searchForm(pii)
	form.Set("action", "apply")
	form.Set("kg:ent_free", "confirm")
	form.Set("chunk:chunk_new", "confirm")
	form.Set("chunk:chunk_nick", "reject")
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), form, withAuthOff); rec.Code != http.StatusOK {
		t.Fatalf("apply: want 200, got %d", rec.Code)
	}
	// A failing candidate search whose error quotes the searched name.
	// Shaped like KGResolver.Candidates: "search %q in %s: %w".
	f.kg.candErr = fmt.Errorf("search %q in p1: %w", pii, errors.New("db down"))
	if rec := resolveDo(s, http.MethodPost, resolvePath(resolveReq), searchForm(pii), withAuthOff); rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d", rec.Code)
	}
	if len(f.audit.rows) < 4 {
		t.Fatalf("expected search and decision audit rows, got %d", len(f.audit.rows))
	}
	sum := sha256.Sum256([]byte(strings.ToLower(pii)))
	forbidden := []string{pii, strings.ToLower(pii), "Quixtrot", hex.EncodeToString(sum[:])[:16],
		"Jane Doe", "invoice", resolveNickname}
	for _, r := range f.audit.rows {
		row := r.Action + " " + r.Target + " " + r.Before + " " + r.After
		for _, bad := range forbidden {
			if strings.Contains(row, bad) {
				t.Errorf("audit row %s carries personal data %q: %s", r.Action, bad, r.After)
			}
		}
	}
	for _, bad := range forbidden {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("a log line carries personal data %q:\n%s", bad, logs.String())
		}
	}
	if !strings.Contains(logs.String(), resolveReq) {
		t.Error("the failure log must still say which request it concerned")
	}
}

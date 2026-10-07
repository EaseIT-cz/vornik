package ui

import (
	"bytes"
	"context"
	"errors"
	"github.com/rs/zerolog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func newSkillRepoUI(t *testing.T) persistence.SkillRepository {
	t.Helper()
	db := sqlitetest.Memory(t)
	return sqlite.NewSkillRepository(db.DB)
}

func TestAdminSkills_ListsDrafts(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "s1", ProjectID: "p1", Name: "trace-hang", Description: "when a model hangs",
		Body: "# steps", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "trace-hang") {
		t.Fatalf("draft skill not rendered in inbox")
	}
}

// TestAdminSkills_RendersFullBody is the 2026-07-08 fix: the skills page
// previously showed only a 400-char truncated preview, so an operator couldn't
// see what they were approving. The full body must now render (in an
// expandable "View full skill" block).
func TestAdminSkills_RendersFullBody(t *testing.T) {
	repo := newSkillRepoUI(t)
	// A body longer than the old 400-char cap, with a marker near the very end
	// that truncation would have cut.
	body := "# Skill\n\n" + strings.Repeat("Detailed instruction line that must be fully visible.\n", 20) + "\nFINAL_STEP_MARKER_XYZ"
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "sfull", ProjectID: "p1", Name: "long-skill", Description: "d",
		Body: body, BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "FINAL_STEP_MARKER_XYZ") {
		t.Errorf("full skill body must render (end marker missing → still truncated)")
	}
	if !strings.Contains(out, "View full skill") {
		t.Errorf("expandable 'View full skill' toggle missing")
	}
}

func TestAdminSkills_ApprovePost(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "s2", ProjectID: "p1", Name: "gate", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	form := url.Values{"id": {"s2"}, "action": {"approve"}, "version": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/skills", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, _ := repo.GetByID(context.Background(), "s2")
	if got.Maturity != persistence.SkillMaturityActive {
		t.Fatalf("approve did not activate: %s", got.Maturity)
	}
}

func TestAdminSkills_GlobalBadgeAndBlastRadius(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "sg", ProjectID: "p1", Name: "global-draft", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft, IsGlobal: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "GLOBAL") {
		t.Error("global draft must render a GLOBAL badge")
	}
	if !strings.Contains(body, "Affects ALL projects") {
		t.Error("global draft must show the blast-radius warning")
	}
	if !strings.Contains(body, "Make project-only") {
		t.Error("global draft must offer a demote button")
	}
}

func TestAdminSkills_SetGlobalPost(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "sp", ProjectID: "p1", Name: "promote", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	form := url.Values{"id": {"sp"}, "action": {"set-global"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/skills", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, _ := repo.GetByID(context.Background(), "sp")
	if !got.IsGlobal {
		t.Fatalf("set-global did not flip the flag")
	}
	if got.Maturity != persistence.SkillMaturityDraft {
		t.Fatalf("set-global must not touch maturity, got %s", got.Maturity)
	}
}

func TestAdminSkills_ShowsActiveSkills(t *testing.T) {
	repo := newSkillRepoUI(t)
	// An approved (active) global skill — the case the drafts-only inbox
	// used to hide.
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "act", ProjectID: "companion-example", Name: "cite-sources", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityActive, IsGlobal: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil) // default = all
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "cite-sources") {
		t.Fatal("active skill must render in the default (all) view")
	}
	if !strings.Contains(body, "GLOBAL") || !strings.Contains(body, "Active") && !strings.Contains(body, "active") {
		t.Errorf("active/global badges must render for the skill")
	}
	// Tabs with counts render.
	if !strings.Contains(body, "Pending review") || !strings.Contains(body, "Active") {
		t.Errorf("maturity tabs must render")
	}
}

func TestAdminSkills_RendersRoles(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "rr", ProjectID: "companion-example", Name: "scoped", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityActive,
		Roles: []string{"researcher", "writer"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A skill with no roles should render "any".
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "ar", ProjectID: "companion-example", Name: "anyrole", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityActive,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "researcher, writer") {
		t.Error("role-scoped skill must render its roles")
	}
	if !strings.Contains(body, "roles <code class=\"text-gray-400\">any") {
		t.Error("no-roles skill must render 'any'")
	}
}

func TestAdminSkills_MaturityFilter(t *testing.T) {
	repo := newSkillRepoUI(t)
	ctx := context.Background()
	_ = repo.Create(ctx, &persistence.Skill{ID: "d1", ProjectID: "p1", Name: "a-draft", Description: "d", Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft})
	_ = repo.Create(ctx, &persistence.Skill{ID: "a1", ProjectID: "p1", Name: "an-active", Description: "d", Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityActive})
	s := NewServer(WithSkillRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/admin/skills?maturity=active", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "an-active") {
		t.Error("active filter must show the active skill")
	}
	if strings.Contains(body, "a-draft") {
		t.Error("active filter must hide the draft")
	}
}

func TestAdminSkills_DefaultHidesRetired(t *testing.T) {
	ctx := context.Background()
	repo := newSkillRepoUI(t)
	_ = repo.Create(ctx, &persistence.Skill{ID: "a1", ProjectID: "p1", Name: "an-active", Description: "d", Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityActive})
	_ = repo.Create(ctx, &persistence.Skill{ID: "r1", ProjectID: "p1", Name: "a-retired", Description: "d", Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityRetired})

	s := NewServer(WithSkillRepository(repo))
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, httptest.NewRequest(http.MethodGet, "/admin/skills", nil))
	body := rec.Body.String()
	if strings.Contains(body, "a-retired") {
		t.Fatal("default skills view must hide retired skills")
	}
	if !strings.Contains(body, "an-active") {
		t.Fatal("default view must show active skills")
	}
	// Retired tab reveals them.
	rec = httptest.NewRecorder()
	s.AdminSkills(rec, httptest.NewRequest(http.MethodGet, "/admin/skills?maturity=retired", nil))
	if !strings.Contains(rec.Body.String(), "a-retired") {
		t.Fatal("Retired tab must show retired skills")
	}
}

func TestAdminSkills_RepoUnwired(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired should still render (200), got %d", rec.Code)
	}
}

// A saved review form must authorize the displayed body, even after a re-proposal.
func TestAdminSkills_SavedRevisionCannotApproveNewProposal(t *testing.T) {
	repo := newSkillRepoUI(t)
	ctx := context.Background()
	draft, err := repo.Upsert(ctx, &persistence.Skill{ID: "saved-review", ProjectID: "p1", Name: "deployment", Body: "v1 body", BodySHA256: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(WithSkillRepository(repo))
	page := httptest.NewRecorder()
	srv.AdminSkills(page, httptest.NewRequest(http.MethodGet, "/ui/admin/skills?maturity=draft", nil))
	if !strings.Contains(page.Body.String(), `name="version" value="1"`) {
		t.Error("rendered v1 review must bind its revision")
	}
	newer, err := repo.Upsert(ctx, &persistence.Skill{ID: "ignored", ProjectID: "p1", Name: "deployment", Body: "v2 body", BodySHA256: "h2"})
	if err != nil || newer.Version != 2 {
		t.Fatalf("reproposal: %v %v", newer, err)
	}
	saved := regexp.MustCompile(`(?s)<form method="POST" action="/ui/admin/skills">.*?</form>`).FindString(page.Body.String())
	form := url.Values{}
	for _, input := range regexp.MustCompile(`name="([^"]+)" value="([^"]*)"`).FindAllStringSubmatch(saved, -1) {
		form.Set(input[1], input[2])
	}
	if form.Get("id") != draft.ID || form.Get("version") != "1" || form.Get("action") != "approve" {
		t.Fatalf("saved review form %v", form)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/skills", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post := httptest.NewRecorder()
	srv.AdminSkills(post, req)
	got, err := repo.GetByID(ctx, draft.ID)
	if err != nil || got.Maturity != persistence.SkillMaturityDraft {
		t.Fatalf("saved v1 form activated v2: %v %v", got, err)
	}
	loc := post.Header().Get("Location")
	if !strings.Contains(loc, "superseded") {
		t.Errorf("stale review must identify newer proposal: %s", loc)
	}
	for _, path := range []string{"/ui/admin/skills", "/ui/admin/skills?maturity=draft"} {
		page = httptest.NewRecorder()
		srv.AdminSkills(page, httptest.NewRequest(http.MethodGet, path, nil))
		body := page.Body.String()
		for _, want := range []string{"v2", "Proposed at", "UTC", "Superseded by v2", "v1 body", "v2 body", "Revision properties"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}

type skillViewFaultRepo struct {
	persistence.SkillRepository
	listErr    bool
	historyErr bool
}

func (r skillViewFaultRepo) ListAcrossProjects(ctx context.Context, m []string, l int) ([]*persistence.Skill, error) {
	if r.listErr {
		return nil, errors.New("list failed")
	}
	return r.SkillRepository.ListAcrossProjects(ctx, m, l)
}
func (r skillViewFaultRepo) ListVersions(ctx context.Context, id string) ([]*persistence.SkillVersion, error) {
	if r.historyErr {
		return nil, errors.New("archive failed")
	}
	return r.SkillRepository.ListVersions(ctx, id)
}
func TestAdminSkillReviewFailuresVisible(t *testing.T) {
	ctx := context.Background()
	repo := newSkillRepoUI(t)
	_, err := repo.Upsert(ctx, &persistence.Skill{ID: "fault", ProjectID: "p", Name: "fault", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fault skillViewFaultRepo
		want  string
	}{{skillViewFaultRepo{SkillRepository: repo, listErr: true}, "failed to load skills"}, {skillViewFaultRepo{SkillRepository: repo, historyErr: true}, "Revision history unavailable"}} {
		srv := NewServer(WithSkillRepository(tc.fault))
		rec := httptest.NewRecorder()
		srv.AdminSkills(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/skills", nil))
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatal(rec.Body.String())
		}
	}
	var logs bytes.Buffer
	srv := NewServer(WithSkillRepository(repo), WithLogger(zerolog.New(&logs)))
	for _, form := range []url.Values{{"id": {"fault"}, "version": {"1"}, "action": {"reject"}}, {"id": {"fault"}, "action": {"approve"}}, {"id": {"missing"}, "version": {"1"}, "action": {"approve"}}, {"id": {"missing"}, "action": {"set-global"}}, {"action": {"approve"}}} {
		req := httptest.NewRequest(http.MethodPost, "/ui/admin/skills", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.AdminSkills(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatal(rec.Code)
		}
	}
	if !strings.Contains(logs.String(), "skill review: decision failed") {
		t.Fatal("storage failure must be logged")
	}
	nilSrv := NewServer()
	rec := httptest.NewRecorder()
	nilSrv.AdminSkills(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/skills", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatal(rec.Code)
	}
	for _, flash := range []string{"superseded", "review-current-proposal", "review-failed"} {
		rec = httptest.NewRecorder()
		srv.AdminSkills(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/skills?done="+flash, nil))
		if strings.Contains(rec.Body.String(), "Last action: "+flash) {
			t.Fatalf("opaque flash %q", flash)
		}
	}
	if skillBodyPreview("long body", 4) != "long…" {
		t.Fatal("body preview")
	}
}

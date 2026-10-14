package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/projectarchive"
)

type failingWiper struct{ err error }

func (f failingWiper) DeleteProjectArtifacts(context.Context, string) (int, error) { return 0, f.err }

// Delta audit 2026-10-14 (lane W8-W, archival design section H). Found at:
// code review; a user observes it through "Delete now". With an artifact wipe
// that failed after the project's rows and YAML were removed, the UI
// redirected to /ui/projects/doomed?delete_failed=1 — a 404, since the
// project was gone. Runs the handler over the real LifecycleServiceAdapter,
// LifecycleService and Sweeper, then renders the page the redirect names.
func TestProjectDeleteNow_ResidueRedirectsToListWithBanner(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "doomed.yaml"), []byte("projectId: doomed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := archivedRegistry()
	sw := projectarchive.NewSweeper(projectarchive.Config{
		Registry:      reg,
		ConfigDir:     dir,
		ArtifactWiper: failingWiper{err: errors.New("s3: access denied on doomed/out/a.bin")},
		Logger:        zerolog.Nop(),
	})
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	lc := &projectarchive.LifecycleService{
		ConfigDir: dir,
		Patcher:   func(c []byte, _ []projectarchive.PatchOp) ([]byte, error) { return c, nil },
		Sweeper:   sw,
		Reload:    func(context.Context) error { go sw.Run(runCtx); return nil },
	}
	s := NewServer(WithProjectRegistry(reg), WithArchiveLifecycle(&LifecycleServiceAdapter{Service: lc}), WithArchiveSweeper(sw))

	rec := httptest.NewRecorder()
	s.projectRouter(rec, httptest.NewRequest(http.MethodPost, "/projects/doomed/delete-now", strings.NewReader("")))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || loc != "/ui/projects?deleted=doomed&residue=artifacts" {
		t.Fatalf("status %d Location %q, want 303 to /ui/projects?deleted=doomed&residue=artifacts", rec.Code, loc)
	}

	page := httptest.NewRecorder()
	s.Projects(page, httptest.NewRequest(http.MethodGet, loc, nil))
	body := page.Body.String()
	if !strings.Contains(body, "doomed was deleted, but some of its artifact files could not be removed") ||
		!strings.Contains(body, "access denied") {
		t.Errorf("projects page does not name the residue:\n%s", excerpt(body, "doomed"))
	}
}

// 2026.10.6 delta audit (#334), follow-up to section H. Found at: code
// review; a user observes it through "Delete now". A deletion whose only
// residue was the registry reload was bannered "some of its artifact files
// could not be removed" although every artifact was removed; the project
// stays listed as archived until the next tick completes the deletion. Runs
// the real handler, adapter, lifecycle and sweeper (reload failing), then the
// page the redirect names.
func TestProjectDeleteNow_ReloadOnlyResidueBannerNamesTheReload(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "doomed.yaml"), []byte("projectId: doomed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := archivedRegistry()
	sw := projectarchive.NewSweeper(projectarchive.Config{
		Registry:  reg,
		ConfigDir: dir,
		Reload:    func(context.Context) error { return errors.New("projects dir: permission denied") },
		Logger:    zerolog.Nop(),
	})
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	lc := &projectarchive.LifecycleService{
		ConfigDir: dir,
		Patcher:   func(c []byte, _ []projectarchive.PatchOp) ([]byte, error) { return c, nil },
		Sweeper:   sw,
		Reload:    func(context.Context) error { go sw.Run(runCtx); return nil },
	}
	s := NewServer(WithProjectRegistry(reg), WithArchiveLifecycle(&LifecycleServiceAdapter{Service: lc}), WithArchiveSweeper(sw))

	rec := httptest.NewRecorder()
	s.projectRouter(rec, httptest.NewRequest(http.MethodPost, "/projects/doomed/delete-now", strings.NewReader("")))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || loc != "/ui/projects?deleted=doomed&residue=reload" {
		t.Fatalf("status %d Location %q, want 303 to /ui/projects?deleted=doomed&residue=reload", rec.Code, loc)
	}

	page := httptest.NewRecorder()
	s.Projects(page, httptest.NewRequest(http.MethodGet, loc, nil))
	body := page.Body.String()
	if strings.Contains(body, "artifact files could not be removed") {
		t.Errorf("a reload-only residue is bannered as artifact residue:\n%s", excerpt(body, "doomed"))
	}
	for _, want := range []string{"registry reload failed", "next tick", "permission denied"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects page lacks %q:\n%s", want, excerpt(body, "doomed was deleted"))
		}
	}
}

// Delta audit 2026-10-14 (lane W8-W). Found at: code review; a user observes
// it through "Delete now" when the wipe outlasts the wait. Pending redirected
// to /ui/projects?deleted=<id>, the same as a finished deletion, and the page
// rendered neither: the operator was told nothing either way.
func TestProjectDeleteNow_PendingRedirectsAndRendersBanner(t *testing.T) {
	rec := deleteNowVia(t, &waitingLifecycle{res: projectarchive.DeleteNowResult{State: projectarchive.DeleteNowPending}})
	loc := rec.Header().Get("Location")
	if loc != "/ui/projects?delete_pending=doomed" {
		t.Fatalf("Location = %q, want /ui/projects?delete_pending=doomed", loc)
	}
	page := httptest.NewRecorder()
	NewServer().Projects(page, httptest.NewRequest(http.MethodGet, loc, nil))
	if !strings.Contains(page.Body.String(), "Deletion of doomed is still running") {
		t.Errorf("projects page does not say the deletion is still running:\n%s", excerpt(page.Body.String(), "doomed"))
	}
}

// Review H round 3, finding 9: with no sweeper that reports, the residue
// banner still says the project was deleted with residue, without detail.
func TestProjects_ResidueBannerWithoutOutcome(t *testing.T) {
	page := httptest.NewRecorder()
	NewServer().Projects(page, httptest.NewRequest(http.MethodGet, "/ui/projects?deleted=doomed&residue=1", nil))
	body := page.Body.String()
	if !strings.Contains(body, "doomed was deleted, but some of its artifact files could not be removed.") {
		t.Errorf("projects page lacks the generic residue banner:\n%s", excerpt(body, "doomed"))
	}
}

func TestProjects_DeletedBanner(t *testing.T) {
	page := httptest.NewRecorder()
	NewServer().Projects(page, httptest.NewRequest(http.MethodGet, "/ui/projects?deleted=doomed", nil))
	if !strings.Contains(page.Body.String(), "Project doomed was deleted.") {
		t.Errorf("projects page does not confirm the deletion")
	}
}

func excerpt(body, needle string) string {
	i := strings.Index(body, needle)
	if i < 0 {
		return "(" + needle + " not in body)"
	}
	lo, hi := max(0, i-300), min(len(body), i+300)
	return body[lo:hi]
}

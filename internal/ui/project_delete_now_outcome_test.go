package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
)

// waitingLifecycle is an ArchiveLifecycle whose delete-now reports a canned
// outcome, as LifecycleServiceAdapter does over the real sweeper.
type waitingLifecycle struct {
	res projectarchive.DeleteNowResult
}

func (w *waitingLifecycle) Archive(context.Context, string, ArchiveLifecycleInput) (ArchiveLifecycleSnapshot, error) {
	return ArchiveLifecycleSnapshot{}, nil
}
func (w *waitingLifecycle) Unarchive(context.Context, string) error { return nil }
func (w *waitingLifecycle) ScheduleDeleteNow(context.Context, string, bool) error {
	return errors.New("the waiting path must be used")
}
func (w *waitingLifecycle) DeleteNowAndWait(context.Context, string, bool, time.Duration) (projectarchive.DeleteNowResult, error) {
	return w.res, nil
}

func archivedRegistry() *registry.Registry {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"doomed": {ID: "doomed", Lifecycle: registry.ProjectLifecycle{
		Status: "archived", ScheduledDeleteAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}})
	return reg
}

func deleteNowVia(t *testing.T, lc ArchiveLifecycle) *httptest.ResponseRecorder {
	t.Helper()
	s := NewServer(WithProjectRegistry(archivedRegistry()), WithArchiveLifecycle(lc))
	rec := httptest.NewRecorder()
	s.projectRouter(rec, httptest.NewRequest(http.MethodPost, "/projects/doomed/delete-now", strings.NewReader("")))
	return rec
}

// GitHub #365: "Delete now" redirected to the project list whether or not the
// wipe worked. A failed wipe must land the operator on the project page, whose
// banner names the failure.
func TestProjectDeleteNow_FailedWipeRedirectsToProjectWithFlag(t *testing.T) {
	rec := deleteNowVia(t, &waitingLifecycle{res: projectarchive.DeleteNowResult{
		State: projectarchive.DeleteNowFailed, Err: errors.New("db wipe: no such table: memory_embed_queue")}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/projects/doomed?delete_failed=1" {
		t.Errorf("Location = %q, want the project page with delete_failed=1", loc)
	}
}

func TestProjectDeleteNow_DeletedRedirectsToList(t *testing.T) {
	rec := deleteNowVia(t, &waitingLifecycle{res: projectarchive.DeleteNowResult{State: projectarchive.DeleteNowDeleted}})
	if loc := rec.Header().Get("Location"); loc != "/ui/projects?deleted=doomed" {
		t.Errorf("Location = %q, want the project list", loc)
	}
}

type reportingSweeper struct{ out projectarchive.DeleteOutcome }

func (r *reportingSweeper) SweepNow(context.Context) {}
func (r *reportingSweeper) LastOutcome(string) (projectarchive.DeleteOutcome, bool) {
	return r.out, true
}

// The banner shows the last failed attempt — including one from the hourly
// tick nobody was watching.
func TestProjectLifecyclePanel_ShowsLastDeleteFailure(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	reg := archivedRegistry()
	s := NewServer(WithProjectRegistry(reg), WithArchiveSweeper(&reportingSweeper{out: projectarchive.DeleteOutcome{
		Seq: 1, At: at, Err: errors.New("db wipe: no such table: memory_embed_queue")}}))
	panel := s.projectLifecyclePanel(reg.GetProject("doomed"))
	if !strings.Contains(panel.LastDeleteError, "memory_embed_queue") || panel.LastDeleteFailedAt == "" {
		t.Fatalf("panel = %+v, want the last failure and its time", panel)
	}
	if !strings.Contains(strings.ToLower(panel.Countdown), "failed") {
		t.Errorf("countdown %q still reads as if the sweeper will simply pick it up", panel.Countdown)
	}

	// A successful last outcome shows no failure.
	s2 := NewServer(WithProjectRegistry(reg), WithArchiveSweeper(&reportingSweeper{out: projectarchive.DeleteOutcome{Seq: 2, At: at}}))
	if p := s2.projectLifecyclePanel(reg.GetProject("doomed")); p.LastDeleteError != "" {
		t.Errorf("success shown as failure: %+v", p)
	}
}

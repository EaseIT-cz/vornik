package ui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Found-at-level regression for GitHub #411 (found at review, 2026-10-16,
// while fixing #408; recorded in cli-design.md's #408 amendment). /ui/tasks
// offered page sizes 10/20/50/100 and no next page: rows past 100 could not be
// reached and nothing said the table was cut — the UI twin of #408, where the
// operator acted on the visible rows only. A user observes it in the web UI,
// so these tests drive the real UI router under its production mount
// (http.StripPrefix("/ui", Server.Handler())) over a migrated SQLite database
// (web-ui-design.md "Task list (/ui/tasks) states its cut — 2026-10-17").

const p411 = "p411"

type fixture411 struct {
	all      []string // every p411 task
	active   []string // p411 tasks in CancellableTaskStatuses
	recent   []string // p411 tasks updated in the last hour
	parent   string   // WAITING_FOR_CHILDREN, the oldest row
	approval string   // AWAITING_APPROVAL
}

// seed411 writes 130 tasks to p411 — an old WAITING_FOR_CHILDREN parent, an
// AWAITING_APPROVAL and a PAUSED task, 40 COMPLETED, 2 FAILED and 85 QUEUED
// (the 10 newest touched within the hour) — and 5 QUEUED tasks to "other".
func seed411(t *testing.T, repo *sqlite.TaskRepository) fixture411 {
	t.Helper()
	ctx := context.Background()
	var f fixture411
	old := time.Now().UTC().Add(-48 * time.Hour)
	n := 0
	mk := func(project string, st persistence.TaskStatus, at time.Time) string {
		t.Helper()
		wf := "wf"
		id := fmt.Sprintf("t411-%s-%03d", project, n)
		n++
		if err := repo.Create(ctx, &persistence.Task{
			ID: id, ProjectID: project, WorkflowID: &wf, Status: st,
			CreationSource: persistence.TaskCreationSourceUser, Priority: 50,
			Payload: []byte(`{"taskType":"t411"}`), CreatedAt: at, UpdatedAt: at,
			MaxAttempts: 3, Attempt: 1,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if project == p411 {
			f.all = append(f.all, id)
			if slices.Contains(persistence.CancellableTaskStatuses(), st) {
				f.active = append(f.active, id)
			}
			if time.Since(at) < time.Hour {
				f.recent = append(f.recent, id)
			}
		}
		return id
	}
	f.parent = mk(p411, persistence.TaskStatusWaitingForChildren, old)
	f.approval = mk(p411, persistence.TaskStatusAwaitingApproval, old.Add(time.Minute))
	mk(p411, persistence.TaskStatusPaused, old.Add(2*time.Minute))
	for i := 0; i < 40; i++ {
		mk(p411, persistence.TaskStatusCompleted, old.Add(time.Duration(10+i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		mk(p411, persistence.TaskStatusFailed, old.Add(time.Duration(60+i)*time.Minute))
	}
	for i := 0; i < 75; i++ {
		mk(p411, persistence.TaskStatusQueued, old.Add(time.Duration(100+i)*time.Minute))
	}
	for i := 0; i < 10; i++ {
		mk(p411, persistence.TaskStatusQueued, time.Now().UTC().Add(-time.Duration(10+i)*time.Minute))
	}
	for i := 0; i < 5; i++ {
		mk("other", persistence.TaskStatusQueued, old.Add(time.Duration(i)*time.Minute))
	}
	if len(f.all) != 130 || len(f.active) != 88 || len(f.recent) != 10 {
		t.Fatalf("fixture: all=%d active=%d recent=%d", len(f.all), len(f.active), len(f.recent))
	}
	return f
}

func newServer411(t *testing.T) (*Server, fixture411) {
	t.Helper()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewTaskRepository(db.DB)
	f := seed411(t, repo)
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))
	return srv, f
}

func get411(ctx context.Context, t *testing.T, srv *Server, url string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	http.StripPrefix("/ui", srv.Handler()).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

var rowID411 = regexp.MustCompile(`name="task_ids" value="([^"]+)"`)

// rows411 returns the task IDs of the desktop table's rows, in order.
func rows411(body string) []string {
	var out []string
	for _, m := range rowID411.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

var relNext411 = regexp.MustCompile(`<a [^>]*href="([^"]+)"[^>]*rel="next"|<a [^>]*rel="next"[^>]*href="([^"]+)"`)

func nextHref411(t *testing.T, body string) string {
	t.Helper()
	m := relNext411.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no rel=next link; the list offers no way past this page")
	}
	h := m[1]
	if h == "" {
		h = m[2]
	}
	return html.UnescapeString(h)
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func TestTasksPage411_StatesTheCutAndPagesPastIt(t *testing.T) {
	srv, f := newServer411(t)

	code, page1 := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&limit=100")
	if code != http.StatusOK {
		t.Fatalf("page 1: %d", code)
	}
	r1 := rows411(page1)
	if len(r1) != 100 {
		t.Fatalf("page 1 shows %d rows, want 100", len(r1))
	}
	if !strings.Contains(page1, "Showing 1–100 of 130 tasks") {
		t.Fatal("page 1 does not state its cut: no \"Showing 1–100 of 130 tasks\"")
	}

	next := nextHref411(t, page1)
	for _, want := range []string{"page=2", "project_id=" + p411, "limit=100"} {
		if !strings.Contains(next, want) {
			t.Errorf("Next link %q drops %s", next, want)
		}
	}
	code, page2 := get411(context.Background(), t, srv, next)
	if code != http.StatusOK {
		t.Fatalf("page 2: %d", code)
	}
	r2 := rows411(page2)
	if len(r2) != 30 {
		t.Fatalf("page 2 shows %d rows, want 30", len(r2))
	}
	if !strings.Contains(page2, "Showing 101–130 of 130 tasks") {
		t.Error("page 2 does not say which rows it shows")
	}
	for _, id := range r2 {
		if slices.Contains(r1, id) {
			t.Errorf("%s is on both pages", id)
		}
	}
	if !sameSet(append(slices.Clone(r1), r2...), f.all) {
		t.Error("the two pages together are not every task of the project")
	}
	if !slices.Contains(r2, f.parent) {
		t.Error("the oldest row (the WAITING_FOR_CHILDREN parent) is not reachable")
	}

	t.Run("a stale page past the end clamps to the last page", func(t *testing.T) {
		_, body := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&limit=100&page=9")
		if !strings.Contains(body, "Showing 101–130 of 130 tasks") || len(rows411(body)) != 30 {
			t.Error("page=9 did not render the last page")
		}
	})

	t.Run("the pagination controls are a labelled nav with real names", func(t *testing.T) {
		if !strings.Contains(page1, `<nav aria-label="Task list pages"`) {
			t.Error("no labelled pagination landmark")
		}
		if !strings.Contains(page1, `aria-disabled="true"`) {
			t.Error("the disabled Prev control on page 1 is not marked aria-disabled")
		}
		if !strings.Contains(page2, `rel="prev"`) {
			t.Error("page 2 has no rel=prev link")
		}
		_, nav, ok := strings.Cut(page1, `<nav aria-label="Task list pages"`)
		nav, _, closed := strings.Cut(nav, "</nav>")
		if !ok || !closed {
			t.Fatal("no complete pagination nav on page 1")
		}
		if strings.Contains(nav, "onclick") || strings.Contains(nav, "onchange") {
			t.Error("pagination controls carry inline handlers")
		}
		if !strings.Contains(nav, "border-control") {
			t.Error("pagination controls do not use the control-border token")
		}
	})
}

func TestTasksPage411_ActiveIsTheCLIActiveSet(t *testing.T) {
	srv, f := newServer411(t)

	code, body := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&active=1&limit=100")
	if code != http.StatusOK {
		t.Fatalf("active page 1: %d", code)
	}
	if !strings.Contains(body, "Showing 1–88 of 88 tasks") {
		t.Fatal("active=1 does not count the CLI --active set (want 88)")
	}
	rows := rows411(body)
	if !sameSet(rows, f.active) {
		t.Fatalf("active=1 lists %d rows that are not the CancellableTaskStatuses set (%d)", len(rows), len(f.active))
	}
	for _, id := range []string{f.parent, f.approval} {
		if !slices.Contains(rows, id) {
			t.Errorf("active=1 hides %s", id)
		}
	}
	if !strings.Contains(body, `aria-label="Filter to Active (unfinished) (88)"`) {
		t.Error("no Active (unfinished) palette cell carrying the set's count")
	}
	if !strings.Contains(body, `hx-trigger="every 10s"`) {
		t.Error("the unfinished view does not auto-refresh")
	}

	code, _ = get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&active=1&status=QUEUED")
	if code != http.StatusBadRequest {
		t.Errorf("active=1 with status= is %d, want 400 (the CLI refuses --active --status)", code)
	}
}

func TestTasksPage411_FiltersSurvive(t *testing.T) {
	srv, f := newServer411(t)

	_, body := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&status=QUEUED&limit=50")
	if !strings.Contains(body, "Showing 1–50 of 85 tasks") {
		t.Fatal("status filter: want \"Showing 1–50 of 85 tasks\"")
	}
	next := nextHref411(t, body)
	if !strings.Contains(next, "status=QUEUED") || !strings.Contains(next, "project_id="+p411) {
		t.Errorf("Next link %q drops the filter", next)
	}
	// The status cells' counts are the project's, so their links keep it.
	cell := regexp.MustCompile(`href="(/ui/tasks\?[^"]*status=COMPLETED[^"]*)"`).FindStringSubmatch(body)
	if cell == nil || !strings.Contains(html.UnescapeString(cell[1]), "project_id="+p411) {
		t.Errorf("the Completed cell's link drops project_id: %v", cell)
	}
	if strings.Contains(html.UnescapeString(cell[1]), "page=") {
		t.Error("a status cell carries page=; a new filter must start at page 1")
	}
	// The page-size form keeps the recency window.
	_, body = get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&updated_within=1h")
	if !strings.Contains(body, "Showing 1–10 of 10 tasks") || !sameSet(rows411(body), f.recent) {
		t.Error("updated_within: the count does not match the windowed list")
	}
	if !strings.Contains(body, `name="updated_within" value="1h"`) {
		t.Error("the page-size form drops updated_within")
	}
}

// countFails411 is the real repository with Count failing — the daemon
// under load. A failed count is never a total (#408 item 8, here item 4).
type countFails411 struct{ *sqlite.TaskRepository }

func (countFails411) Count(context.Context, persistence.TaskFilter) (int64, error) {
	return 0, errors.New("count timed out")
}

func TestTasksPage411_UnknownTotalIsNeverATotal(t *testing.T) {
	db := sqlitetest.Memory(t)
	repo := sqlite.NewTaskRepository(db.DB)
	seed411(t, repo)
	srv := NewServer(WithTaskRepository(countFails411{repo}), WithOnboardingDetector(alreadyOnboardedDetector()))

	_, body := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411)
	if len(rows411(body)) != 20 {
		t.Fatalf("rows = %d, want 20", len(rows411(body)))
	}
	if !strings.Contains(body, "Showing 1–20; the total could not be counted") {
		t.Error("a failed count does not say so")
	}
	if strings.Contains(body, "Showing 1–20 of") {
		t.Error("a failed count printed a total")
	}
	if next := nextHref411(t, body); !strings.Contains(next, "page=2") {
		t.Errorf("a full page with an unknown total offers no Next: %q", next)
	}
}

func TestTasksPage411_ScopedSessionCountsItsProjectsNotThePage(t *testing.T) {
	srv, _ := newServer411(t)
	ctx := api.ContextWithScopeForTesting(context.Background(), p411, "other")

	_, body := get411(ctx, t, srv, "/ui/tasks")
	if !strings.Contains(body, "Showing 1–20 of 135 tasks") {
		t.Error("scoped session: the list's total is not the allowed projects' count (want 135)")
	}
	// 85 queued in p411 plus 5 in other, not the queued rows on a 20-row page.
	if !strings.Contains(body, `aria-label="Filter to Queued (90)"`) {
		t.Error("scoped session: the Queued tile counts the page, not the projects (want 90)")
	}
}

func TestTasksPage411_PageLocalSearchAndSortAreSaid(t *testing.T) {
	srv, _ := newServer411(t)

	_, body := get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&q=t411-p411-12")
	if !strings.Contains(body, "the search covers this page only") {
		t.Error("q narrows the page without saying it covers this page only")
	}
	_, body = get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411+"&sort=priority&dir=asc")
	if !strings.Contains(body, "sorted within this page") {
		t.Error("a non-default sort over several pages does not say it is page-local")
	}
	_, body = get411(context.Background(), t, srv, "/ui/tasks?project_id="+p411)
	if strings.Contains(body, "sorted within this page") {
		t.Error("the default order is the list's own; no page-local note expected")
	}
}

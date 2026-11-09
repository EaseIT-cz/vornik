package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #603 (web UI design, "The overview answers 'does anything need me
// now?' first", 2026-11-07). The overview's "needs you" counts came from
// different queries than the lists they linked to (2026-08-14: a failures card
// counted one set and linked to another), it showed "All clear" while
// approvals waited, and agent approvals and doctor errors were not on it.

func overviewTasks(n map[persistence.TaskStatus]int) []*persistence.Task {
	now := time.Now()
	var out []*persistence.Task
	for st, k := range n {
		for i := 0; i < k; i++ {
			out = append(out, &persistence.Task{ID: fmt.Sprintf("t-%s-%d", strings.ToLower(string(st)), i),
				ProjectID: "p1", Status: st, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)})
		}
	}
	return out
}

func overviewServer(tasks []*persistence.Task, opts ...ServerOption) *Server {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(tasks, f), nil
		},
	}
	return NewServer(append([]ServerOption{WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector())}, opts...)...)
}

func renderOverview(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestDashboard_603_NeedsYouIsTheInboxCount(t *testing.T) {
	tasks := overviewTasks(map[persistence.TaskStatus]int{
		persistence.TaskStatusAwaitingApproval: 3,
		persistence.TaskStatusAwaitingInput:    2,
		persistence.TaskStatusFailed:           2,
	})
	srv := overviewServer(tasks, WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) {
		return []persistence.AgentApprovalRequestRow{{ID: "apr_1", Kind: "widening_change", Status: "pending", CreatedAt: time.Now()}}, nil
	}))
	inbox := srv.inboxData(httptest.NewRequest(http.MethodGet, "/inbox", nil), true)
	if inbox.Count != 8 {
		t.Fatalf("fixture: the inbox counts %d, want 8 (7 rows + 1 agent approval)", inbox.Count)
	}
	body := renderOverview(t, srv)
	for _, want := range []string{"Needs you · 8", "See all 8 in Needs you", "(8)</span>", "2 more in Needs you",
		"1 agent request waiting on your approver device"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if n := strings.Count(body, `id="inbox-item-`); n != overviewNeedsYouRows {
		t.Errorf("overview shows %d inbox rows, want %d", n, overviewNeedsYouRows)
	}
	// The rows are the inbox's, with its actions: an approval row carries
	// the approve control the inbox renders.
	if !strings.Contains(body, "/ui/tasks/t-awaiting_approval-") {
		t.Error("an approval row must link to its task, as in the inbox")
	}
	if strings.Contains(body, "All clear") {
		t.Error("All clear while eight things wait")
	}
	// The list "See all" links to holds what the count counted (the
	// 2026-08-14 failures card counted one set and linked to another): the
	// inbox page renders the 7 task rows, and the agent approval as its card.
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/inbox", nil))
	if n := strings.Count(rec.Body.String(), `id="inbox-item-`); n != 7 {
		t.Errorf("the inbox page renders %d task rows, want the 7 the count includes", n)
	}
}

// The Projects page is open to project-scoped sessions; the moved tiles keep
// to what the session may see (the overview they came from was all-access).
func TestProjects_603_ScopedSessionSeesItsOwnOnly(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	if p := srv.projectReg.GetProject("p1"); p != nil {
		p.Autonomy.Enabled = true
	}
	srv.autonomyEvalRepo = &fakeAutonomyEvalRepo{}
	srv.activeChatSource = &fakeActiveChatSource{count: 7}
	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "other-project"))
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, `href="/ui/projects/p1" class="text-ink-200 font-medium truncate"`) {
		t.Error("a session scoped to another project sees p1's autonomy row")
	}
	if strings.Contains(body, ">7<") {
		t.Error("a scoped session sees the instance-wide chat count")
	}
}

func TestDashboard_603_AllClearOnlyWhenNothingWaits(t *testing.T) {
	if body := renderOverview(t, overviewServer(nil)); !strings.Contains(body, "All clear") {
		t.Error("an empty overview must say All clear")
	}
	// Red before #603: approvals alone left "All clear" on the page.
	body := renderOverview(t, overviewServer(overviewTasks(map[persistence.TaskStatus]int{persistence.TaskStatusAwaitingApproval: 1})))
	if strings.Contains(body, "All clear") {
		t.Error("an approval waits, so the overview must not say All clear")
	}
}

func TestDashboard_603_WaitingForChildrenIsNotAProblem(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{persistence.TaskStatusWaitingForChildren: 3}, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))
	body := renderOverview(t, srv)
	if strings.Contains(body, "waiting for children") || strings.Contains(body, `data-block="health"`) {
		t.Error("a parent waiting on its children is not a health problem")
	}
}

func TestDashboard_603_DoctorErrorsFromTheLastReport(t *testing.T) {
	var names []string
	var at time.Time
	ok := false
	srv := overviewServer(nil, WithLastDoctorErrors(func() ([]string, time.Time, bool) { return names, at, ok }))
	if body := renderOverview(t, srv); strings.Contains(body, `data-line="doctor"`) || !strings.Contains(body, "All clear") {
		t.Error("before any doctor report the overview states nothing about doctor")
	}
	names, at, ok = []string{"model_health", "fallback_rungs"}, time.Now().Add(-2*time.Hour), true
	body := renderOverview(t, srv)
	for _, want := range []string{`data-line="doctor"`, "Doctor: 2 errors", "model_health, fallback_rungs", "2h 0m ago", "vornikctl doctor"} {
		if !strings.Contains(body, want) {
			t.Errorf("doctor line lacks %q", want)
		}
	}
	if strings.Contains(body, "All clear") {
		t.Error("doctor errors are not all clear")
	}
	names = nil // a clean report
	if body := renderOverview(t, srv); strings.Contains(body, `data-line="doctor"`) || !strings.Contains(body, "All clear") {
		t.Error("a clean report shows no doctor line")
	}
}

func TestProjects_603_NextEvalAndActiveChats(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	if p := srv.projectReg.GetProject("p1"); p != nil {
		p.Autonomy.Enabled = true
		p.Autonomy.PollInterval = "10m"
	}
	srv.autonomyEvalRepo = &fakeAutonomyEvalRepo{rows: []*persistence.AutonomyEvaluation{
		{ProjectID: "p1", Outcome: "noop", CreatedAt: time.Now().Add(-2 * time.Minute)},
	}}
	srv.activeChatSource = &fakeActiveChatSource{count: 5}
	rec := httptest.NewRecorder()
	srv.Projects(rec, httptest.NewRequest(http.MethodGet, "/projects", nil))
	body := rec.Body.String()
	for _, want := range []string{`id="autonomy-etas"`, "every 10m0s", "noop", `data-block="active-chats"`, ">5<", "autonomy-eta-label"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects page lacks %q", want)
		}
	}
	// The overview's At a glance section shows it too (§D), from the same loader.
	if body := renderOverview(t, srv); !strings.Contains(body, `id="autonomy-etas"`) {
		t.Error("the overview's At a glance section lost the autonomy list")
	}
}

// §D (operator correction 2026-11-07): the statistics return as an At a
// glance section, below Needs you and Health, not instead of them.
func TestDashboard_603_AtAGlanceSitsBelowNeedsYou(t *testing.T) {
	srv := overviewServer(overviewTasks(map[persistence.TaskStatus]int{persistence.TaskStatusAwaitingApproval: 1}),
		WithActiveChatSource(&fakeActiveChatSource{count: 3}))
	body := renderOverview(t, srv)
	needs, glance := strings.Index(body, `data-block="needs-you"`), strings.Index(body, `data-block="at-a-glance"`)
	if needs < 0 || glance < 0 || glance < needs {
		t.Fatalf("Needs you at %d, At a glance at %d: the statistics must render, below Needs you", needs, glance)
	}
	for _, want := range []string{"Active Chats", "Autonomy · next eval", "Memory pipeline", "Judge · last 24h"} {
		if !strings.Contains(body[glance:], want) {
			t.Errorf("At a glance lacks %q", want)
		}
	}
}

// Security review 2026-11-08: At a glance's instance-wide aggregates (task
// counts, active tasks, chats, judge, ingest backlog) have no project filter,
// so a project-scoped session reaching the overview gets none of them, and
// the autonomy list keeps to its projects.
func TestDashboard_603_ScopedSessionSeesNoInstanceWideStats(t *testing.T) {
	tasks := []*persistence.Task{{ID: "t-other-running", ProjectID: "other", Status: persistence.TaskStatusRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	srv := overviewServer(tasks, WithActiveChatSource(&fakeActiveChatSource{count: 9}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "mine"))
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "t-other-running") || strings.Contains(body, ">9<") {
		t.Fatal("a scoped session sees another project's active task or the instance-wide chat count")
	}
	if !strings.Contains(body, `data-line="glance-restricted"`) {
		t.Fatal("the page must say why the statistics are empty")
	}
}

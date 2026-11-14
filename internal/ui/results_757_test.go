package ui

// Results & cost (ui-design-refresh-design.md §15, #757). Before it the
// Results & cost door landed on /ui/spend, which answers "where did the
// money go" with tables, not "what got done and what did it cost".

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type resultsUsage struct {
	*fakeUsageRepo
	byProject  []persistence.ProjectSpend
	days       map[string][]persistence.DailySpend // by project; "" = all
	aggErr     error
	seriesErr  error
	seriesUsed []string
}

func (f *resultsUsage) AggregateByProject(_ context.Context, _, _ time.Time, _ int) ([]persistence.ProjectSpend, error) {
	return f.byProject, f.aggErr
}

func (f *resultsUsage) TimeSeriesByDay(_ context.Context, _, _ time.Time, projectID string) ([]persistence.DailySpend, error) {
	f.seriesUsed = append(f.seriesUsed, projectID)
	return f.days[projectID], f.seriesErr
}

func utcDay(back int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -back)
}

func resultsServer(usage *resultsUsage, jobs map[string]int64) *Server {
	return resultsServerErr(usage, jobs, nil)
}

func resultsServerErr(usage *resultsUsage, jobs map[string]int64, jobsErr error) *Server {
	execs := &mocks.MockExecutionRepository{CountCompletedByProjectFunc: func(context.Context, time.Time, time.Time) (map[string]int64, error) {
		return jobs, jobsErr
	}}
	return NewServer(WithLLMUsageRepository(usage), WithExecutionRepository(execs), WithOnboardingDetector(alreadyOnboardedDetector()))
}

func renderResults(t *testing.T, srv *Server, r *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Results(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("results: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestResults_757_SentenceTableAndChartAgree(t *testing.T) {
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{},
		byProject: []persistence.ProjectSpend{{ProjectID: "office", CostUSD: 9.10}, {ProjectID: "code", CostUSD: 7.80}, {ProjectID: "", CostUSD: 0.50}, {ProjectID: "idle", CostUSD: 0}},
		days:      map[string][]persistence.DailySpend{"": {{Day: utcDay(2), CostUSD: 10.00}, {Day: utcDay(0), CostUSD: 7.40}}},
	}
	body := renderResults(t, resultsServer(usage, map[string]int64{"office": 92, "code": 31}), httptest.NewRequest(http.MethodGet, "/results", nil))
	for _, want := range []string{"Last 14 days: 123 jobs done, $17.40 spent.", `class="c-chart"`, "office", "code", "No project", "$9.10", `href="/ui/spend"`} {
		if !strings.Contains(body, want) {
			t.Errorf("results lacks %q", want)
		}
	}
	if strings.Contains(body, ">idle<") {
		t.Error("a project with neither jobs nor cost should be left out")
	}
	if strings.Index(body, ">office<") > strings.Index(body, ">code<") {
		t.Error("rows sort by cost, highest first")
	}
	// The chart's series total equals the sentence's cost ($17.40), which
	// equals the table's cost column: 9.10 + 7.80 + 0.50.
	if !strings.Contains(body, "Highest: $10.00 on") || !strings.Contains(body, "14-day total: $17.40.") {
		t.Error("the chart should draw the daily series and print its total")
	}
}

func TestResults_757_EmptyWindow(t *testing.T) {
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{}, days: map[string][]persistence.DailySpend{}}
	body := renderResults(t, resultsServer(usage, map[string]int64{}), httptest.NewRequest(http.MethodGet, "/results", nil))
	for _, want := range []string{"Last 14 days: no jobs done and nothing spent.", "Nothing recorded in the last 14 days.", `class="v-empty"`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty window lacks %q", want)
		}
	}
}

func TestResults_757_ScopedSessionSeesOnlyItsProjects(t *testing.T) {
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{},
		byProject: []persistence.ProjectSpend{{ProjectID: "mine", CostUSD: 2.00}, {ProjectID: "theirs", CostUSD: 99.00}, {ProjectID: "", CostUSD: 5.00}},
		days:      map[string][]persistence.DailySpend{"mine": {{Day: utcDay(0), CostUSD: 2.00}}, "": {{Day: utcDay(0), CostUSD: 106.00}}},
	}
	req := httptest.NewRequest(http.MethodGet, "/results", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "mine"))
	body := renderResults(t, resultsServer(usage, map[string]int64{"mine": 3, "theirs": 50}), req)
	for _, leak := range []string{"theirs", "$99.00", "No project", "$106.00", "53 jobs"} {
		if strings.Contains(body, leak) {
			t.Errorf("a scoped session sees %q", leak)
		}
	}
	if !strings.Contains(body, "Last 14 days: 3 jobs done, $2.00 spent.") || !strings.Contains(body, "14-day total: $2.00.") {
		t.Error("the scoped sentence, table and chart should total the session's projects")
	}
	if strings.Join(usage.seriesUsed, ",") != "mine" {
		t.Errorf("the scoped chart should query only the session's projects, queried %v", usage.seriesUsed)
	}
}

func TestResults_757_AFailingQueryOmitsTheSentence(t *testing.T) {
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{}, aggErr: errors.New("db down"), days: map[string][]persistence.DailySpend{}}
	body := renderResults(t, resultsServer(usage, map[string]int64{"office": 4}), httptest.NewRequest(http.MethodGet, "/results", nil))
	if strings.Contains(body, "Last 14 days:") {
		t.Error("no total may be shown from a partial set of queries")
	}
	if !strings.Contains(body, "Results could not be loaded right now.") || strings.Contains(body, ">office<") {
		t.Error("the table whose query failed shows its empty state, not a partial table")
	}
	if !strings.Contains(body, `class="c-chart"`) {
		t.Error("the chart's own query succeeded and still draws")
	}
}

func TestResults_757_EachFailingQueryOmitsTheSentence(t *testing.T) {
	spend := []persistence.ProjectSpend{{ProjectID: "office", CostUSD: 1}}
	// The jobs query fails: no sentence, the table says so, the chart draws.
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{}, byProject: spend, days: map[string][]persistence.DailySpend{"": {{Day: utcDay(0), CostUSD: 1}}}}
	body := renderResults(t, resultsServerErr(usage, nil, errors.New("db down")), httptest.NewRequest(http.MethodGet, "/results", nil))
	if strings.Contains(body, "Last 14 days:") || !strings.Contains(body, "Results could not be loaded right now.") || !strings.Contains(body, `class="c-chart"`) {
		t.Error("a failed jobs query: sentence omitted, table empty state, chart still drawn")
	}
	// The daily query fails: no sentence, the chart says so, the table draws.
	usage = &resultsUsage{fakeUsageRepo: &fakeUsageRepo{}, byProject: spend, seriesErr: errors.New("db down")}
	body = renderResults(t, resultsServer(usage, map[string]int64{"office": 2}), httptest.NewRequest(http.MethodGet, "/results", nil))
	if strings.Contains(body, "Last 14 days:") || !strings.Contains(body, "Spend could not be loaded right now.") || strings.Contains(body, `class="c-chart"`) || !strings.Contains(body, ">office<") {
		t.Error("a failed daily query: sentence omitted, chart empty state, table still drawn")
	}
}

// A session scoped to no project (awaiting access) sees nothing. The auth
// middleware stamps it with the no-access sentinel project (middleware.go,
// noAccessSentinelProject), which matches no real project.
func TestResults_757_NoProjectSessionSeesNothing(t *testing.T) {
	usage := &resultsUsage{fakeUsageRepo: &fakeUsageRepo{},
		byProject: []persistence.ProjectSpend{{ProjectID: "theirs", CostUSD: 99}, {ProjectID: "", CostUSD: 5}},
		days:      map[string][]persistence.DailySpend{"": {{Day: utcDay(0), CostUSD: 104}}},
	}
	req := httptest.NewRequest(http.MethodGet, "/results", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "\x00no-access"))
	body := renderResults(t, resultsServer(usage, map[string]int64{"theirs": 50}), req)
	for _, leak := range []string{"theirs", "$99.00", "No project", "$104.00"} {
		if strings.Contains(body, leak) {
			t.Errorf("a session with no project sees %q", leak)
		}
	}
	if !strings.Contains(body, "Last 14 days: no jobs done and nothing spent.") {
		t.Error("a session with no project should read the empty sentence")
	}
}

func TestNav_757_ResultsLandsOnItsPage(t *testing.T) {
	for _, a := range navModel() {
		if a.Key == "results" {
			if a.Href != "/ui/results" || len(a.Dests) != 4 || a.Dests[0].Key != "results" {
				t.Errorf("Results & cost should land on /ui/results with Results first: %+v", a)
			}
		}
	}
	if navAreaForPage("results") != "results" {
		t.Error("pageToArea should map the Results page to its door")
	}
}

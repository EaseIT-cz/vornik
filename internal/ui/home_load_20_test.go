package ui

// Home loads only what it shows (ui-design-refresh-design.md §20, operator
// report 2026-11-16: Home opened noticeably slower than any other menu item
// on the phone). Measured on the deployed daemon: /ui/ 0.51-1.46 s against
// 2-10 ms for /ui/tasks and /ui/projects. Home ran six instance-wide SumCost
// calls and listed up to 5,000 usage rows for a sparkline it does not show.
//
// Also found while measuring (2026-11-16, observed on the deployed daemon):
// Home said "nothing in flight" while tasks ran, because its count came from
// TaskCounts, which only the Diagnostics loader fills.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// countingUsageRepo answers SumCost by window and counts the calls Home
// makes.
type countingUsageRepo struct {
	stubLLMUsageRepo
	day, week float64
	sums      atomic.Int32
	lists     atomic.Int32
}

func (c *countingUsageRepo) SumCost(_ context.Context, since, _ time.Time) (float64, error) {
	c.sums.Add(1)
	if time.Since(since) < 48*time.Hour {
		return c.day, nil
	}
	return c.week, nil
}

func (c *countingUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	c.lists.Add(1)
	return nil, nil
}

func TestHome_20_TwoSpendSumsAndNoUsageRows(t *testing.T) {
	usage := &countingUsageRepo{day: 4, week: 10} // 4 / (10/7) = 2.8
	srv := overviewServer(nil, WithLLMUsageRepository(usage))
	body := renderOverview(t, srv)
	if n := usage.sums.Load(); n != 2 {
		t.Errorf("Home made %d SumCost calls; it shows two figures (24 h, 7 d) and the spike reads the same two", n)
	}
	if n := usage.lists.Load(); n != 0 {
		t.Errorf("Home listed usage rows %d time(s); it shows no sparkline", n)
	}
	for _, want := range []string{"$4.00", "$10.00 over 7 days", "Spend spike: the last 24 hours are 2.8×"} {
		if !strings.Contains(body, want) {
			t.Errorf("Home lacks %q", want)
		}
	}
}

func TestHome_20_InFlightCountsRunningLeasedAndQueued(t *testing.T) {
	var seen persistence.TaskFilter
	counts := 0
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
		CountFunc: func(_ context.Context, f persistence.TaskFilter) (int64, error) {
			if len(f.Statuses) > 0 {
				seen, counts = f, counts+1
				return 3, nil
			}
			return 0, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))
	body := renderOverview(t, srv)
	if !strings.Contains(body, "3 in flight") {
		t.Errorf("Home should read 3 in flight from one exact count")
	}
	if counts != 1 {
		t.Fatalf("Home counted in-flight tasks %d times, want 1", counts)
	}
	want := map[persistence.TaskStatus]bool{persistence.TaskStatusRunning: true, persistence.TaskStatusLeased: true, persistence.TaskStatusQueued: true}
	if len(seen.Statuses) != len(want) {
		t.Fatalf("in-flight statuses = %v", seen.Statuses)
	}
	for _, st := range seen.Statuses {
		if !want[st] {
			t.Errorf("in-flight count includes %s", st)
		}
	}
}

// The Diagnostics overview keeps its full spend tile: the 30-day figure and
// the sparkline rows still load there.
func TestHome_20_DiagnosticsKeepsTheSpendTile(t *testing.T) {
	usage := &countingUsageRepo{day: 3.5, week: 10}
	srv := overviewServer(nil, WithLLMUsageRepository(usage))
	renderDiagnosticsOverview(t, srv)
	if usage.lists.Load() == 0 || usage.sums.Load() < 4 {
		t.Errorf("Diagnostics loaded %d sums and %d lists; its tile shows 24 h, 7 d, 30 d and the sparkline",
			usage.sums.Load(), usage.lists.Load())
	}
}

func renderDiagnosticsOverview(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.DiagnosticsOverview(rec, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostics overview: %d", rec.Code)
	}
	return rec.Body.String()
}

// One icon per page (§20.2 item 3): two nav entries share an icon only when
// they open the same page (an area's door and its landing entry).
func TestNav_20_EveryPageHasItsOwnIcon(t *testing.T) {
	hrefOf := map[string]string{} // icon -> href
	label := map[string]string{}
	for _, area := range navModel() {
		entries := []struct{ key, href, icon string }{{area.Key, area.Href, area.Icon}}
		for _, d := range area.Dests {
			entries = append(entries, struct{ key, href, icon string }{d.Key, d.Href, d.Icon})
		}
		for _, e := range entries {
			if e.icon == "" {
				t.Errorf("nav entry %s has no icon", e.key)
				continue
			}
			if h, ok := hrefOf[e.icon]; ok && h != e.href {
				t.Errorf("%s (%s) and %s (%s) share the icon %s", label[e.icon], h, e.key, e.href, e.icon)
				continue
			}
			hrefOf[e.icon], label[e.icon] = e.href, e.key
		}
	}
}

package ui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// homeRecentRows is how many finished jobs the home page lists (§14.2).
const homeRecentRows = 5

// homeRecent is one row of the home page's "Recently done".
type homeRecent struct {
	TaskID    string
	Summary   string
	ProjectID string
	When      string
}

// loadHome fills the home page's own blocks (ui-design-refresh-design.md
// §14.2, #756): the greeting, the three stats and Recently done. Instance-
// wide figures load only for an all-access session; the list keeps to the
// session's projects otherwise.
func (s *Server) loadHome(ctx context.Context, r *http.Request, data *DashboardData) {
	now := time.Now()
	data.Greeting = greetingFor(now)
	defer func() { data.StatusLine = homeStatusLine(data.NeedsYouCount, data.DoneToday, data.DoneTodayShown) }()
	allAccess := requestHasAllProjectAccess(r)
	data.Spend = s.loadSpendWindows(ctx, r)
	if allAccess {
		s.loadActiveTasks(ctx, data)
	}
	for _, eta := range s.loadAutonomyETAs(ctx) {
		if api.RequestAllowsProject(r, eta.ProjectID) {
			e := eta
			data.NextScheduled = &e
			break
		}
	}
	if s.execRepo == nil {
		return
	}
	completed := persistence.ExecutionStatusCompleted
	if allAccess {
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		if n, err := s.execRepo.Count(ctx, persistence.ExecutionFilter{Status: &completed, CompletedSince: &midnight}); err == nil {
			data.DoneToday, data.DoneTodayShown = n, true
		}
	}
	filter := persistence.ExecutionFilter{Status: &completed, NewestCompletedFirst: true, PageSize: homeRecentRows}
	if scoped, ok := api.RequestScopedProjects(r); ok {
		filter.ProjectIDs = scoped
	} else if !allAccess {
		// Neither all-access nor a scoped project list (an unscoped,
		// non-admin caller): show no results rather than guess a scope.
		return
	}
	execs, err := s.execRepo.List(ctx, filter)
	if err != nil {
		return
	}
	for _, e := range execs {
		row := homeRecent{TaskID: e.TaskID, ProjectID: e.ProjectID, Summary: e.TaskID}
		if s.taskRepo != nil {
			if t, err := s.taskRepo.Get(ctx, e.TaskID); err == nil && t != nil {
				if sum := taskSummary(t.Payload); sum != "" {
					row.Summary = sum
				}
			}
		}
		if at := completedAt(e); !at.IsZero() {
			row.When = humanizeAgo(now.Sub(at))
		}
		data.RecentDone = append(data.RecentDone, row)
	}
}

func completedAt(e *persistence.Execution) time.Time {
	if e.CompletedAt != nil {
		return *e.CompletedAt
	}
	return e.CreatedAt
}

// homeStatusLine is the sentence under the greeting: what needs you, then
// what got done.
func homeStatusLine(needs int, done int64, doneShown bool) string {
	line := "Nothing needs you right now."
	switch {
	case needs == 1:
		line = "1 thing needs you."
	case needs > 1:
		line = fmt.Sprintf("%d things need you.", needs)
	}
	switch {
	case doneShown && done == 1:
		line += " 1 job done today."
	case doneShown && done > 1:
		line += fmt.Sprintf(" %d jobs done today.", done)
	}
	return line
}

func greetingFor(t time.Time) string {
	switch h := t.Hour(); {
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

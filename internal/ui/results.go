package ui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
)

// resultsDays is the Results page's window (ui-design-refresh-design.md
// §15.2, #757): fourteen UTC calendar days, today partial.
const resultsDays = 14

// resultsRow is one row of the by-assistant table.
type resultsRow struct {
	ProjectID string
	Name      string
	Jobs      int64
	Cost      float64
}

// ResultsData backs results.html.
type ResultsData struct {
	Title       string
	CurrentPage string
	Days        int
	Summary     string // empty when any query failed (§15.2)
	Chart       barChart
	ChartShown  bool
	ChartTotal  string // the series sum, which equals the sentence's cost
	Rows        []resultsRow
	TableShown  bool
}

// Results answers "what got done and what did it cost" for the last 14
// days: one sentence, one daily-spend chart and a table by assistant
// (design §15.2). Cost by project, jobs done by project and daily spend
// each come from one query; all three share one [since, until) window, so
// the sentence, the table and the chart agree.
func (s *Server) Results(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data := ResultsData{Title: "Results & cost", CurrentPage: "results", Days: resultsDays}

	now := time.Now().UTC()
	since := now.Truncate(24*time.Hour).AddDate(0, 0, -(resultsDays - 1))
	scope := newResultsScope(r)

	// failures counts the queries that did not answer; the sentence shows
	// only when all three did (a total from a partial set would mislead).
	failures := 0
	if s.llmUsageRepo == nil || s.execRepo == nil {
		failures++
	}
	rows, failed := s.resultsTable(ctx, since, now, scope)
	failures += failed
	data.TableShown = failed == 0 && s.llmUsageRepo != nil && s.execRepo != nil

	if s.llmUsageRepo != nil {
		daily, err := s.resultsDaily(ctx, since, now, scope)
		if err != nil {
			s.logger.Warn().Err(err).Msg("results: daily spend")
			failures++
		} else {
			var total float64
			data.Chart, total = resultsChart(daily, since)
			data.ChartShown, data.ChartTotal = true, dollarUnit(total)
		}
	}

	var jobs int64
	var cost float64
	for _, rw := range rows {
		jobs += rw.Jobs
		cost += rw.Cost
	}
	if data.TableShown {
		data.Rows = rows
	}
	if failures == 0 {
		data.Summary = resultsSentence(jobs, cost)
	}
	s.render(w, "results.html", data)
}

// resultsScope is what the session may see: every project, or its list.
type resultsScope struct {
	allAccess bool
	scoped    []string
}

func newResultsScope(r *http.Request) resultsScope {
	scoped, isScoped := api.RequestScopedProjects(r)
	return resultsScope{allAccess: !isScoped && requestHasAllProjectAccess(r), scoped: scoped}
}

// allows reports whether a row for project id may be shown. Spend without
// a project ("") is shown only to an all-access session.
func (sc resultsScope) allows(id string) bool {
	if sc.allAccess {
		return true
	}
	return id != "" && slices.Contains(sc.scoped, id)
}

// resultsTable joins cost by project and jobs done by project into the
// table's rows, the session's projects only, sorted by cost (highest
// first), leaving out a project with neither. It returns how many of its
// two queries failed.
func (s *Server) resultsTable(ctx context.Context, since, until time.Time, scope resultsScope) ([]resultsRow, int) {
	failed := 0
	byID := map[string]*resultsRow{}
	row := func(id string) *resultsRow {
		if byID[id] == nil {
			byID[id] = &resultsRow{ProjectID: id, Name: s.resultsName(id)}
		}
		return byID[id]
	}
	if s.llmUsageRepo != nil {
		spend, err := s.llmUsageRepo.AggregateByProject(ctx, since, until, 0)
		if err != nil {
			s.logger.Warn().Err(err).Msg("results: cost by project")
			failed++
		}
		for _, p := range spend {
			if scope.allows(p.ProjectID) {
				row(p.ProjectID).Cost += p.CostUSD
			}
		}
	}
	if s.execRepo != nil {
		jobs, err := s.execRepo.CountCompletedByProject(ctx, since, until)
		if err != nil {
			s.logger.Warn().Err(err).Msg("results: jobs done by project")
			failed++
		}
		for id, n := range jobs {
			if scope.allows(id) {
				row(id).Jobs += n
			}
		}
	}
	var rows []resultsRow
	for _, rw := range byID {
		if rw.Jobs != 0 || rw.Cost != 0 {
			rows = append(rows, *rw)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Cost != rows[j].Cost {
			return rows[i].Cost > rows[j].Cost
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, failed
}

// resultsDaily sums the daily spend for what the session may see: one
// global query for all access, one per project for a scoped session (the
// buckets carry no project to filter on afterwards).
func (s *Server) resultsDaily(ctx context.Context, since, until time.Time, scope resultsScope) (map[string]float64, error) {
	ids := scope.scoped
	if scope.allAccess {
		ids = []string{""}
	}
	out := map[string]float64{}
	for _, id := range ids {
		days, err := s.llmUsageRepo.TimeSeriesByDay(ctx, since, until, id)
		if err != nil {
			return nil, err
		}
		for _, d := range days {
			out[d.Day.UTC().Format("2006-01-02")] += d.CostUSD
		}
	}
	return out, nil
}

// resultsChart lays out the 14 daily buckets, zero-filled, and returns the
// series total the page prints under the chart.
func resultsChart(daily map[string]float64, since time.Time) (barChart, float64) {
	vals, dates, tips := make([]float64, resultsDays), make([]string, resultsDays), make([]string, resultsDays)
	total := 0.0
	for i := range resultsDays {
		d := since.AddDate(0, 0, i).Format("2006-01-02")
		vals[i], dates[i] = daily[d], d
		tips[i] = fmt.Sprintf("%s: %s", d, dollarUnit(daily[d]))
		total += daily[d]
	}
	return newBarChart(vals, dates, tips, dollarUnit), total
}

func resultsSentence(jobs int64, cost float64) string {
	if jobs == 0 && cost == 0 {
		return fmt.Sprintf("Last %d days: no jobs done and nothing spent.", resultsDays)
	}
	return fmt.Sprintf("Last %d days: %s done, %s spent.", resultsDays, countUnit("job", "jobs")(float64(jobs)), dollarUnit(cost))
}

func (s *Server) resultsName(id string) string {
	if id == "" {
		return "No project"
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(id); p != nil && p.DisplayName != "" {
			return p.DisplayName
		}
	}
	return id
}

package ui

// The overview tiles that moved to their detail pages (web UI design, "The
// overview answers 'does anything need me now?' first", 2026-11-07, GitHub
// #603): the four spend windows to /ui/spend and the autonomy next-eval list
// to /ui/projects. The loaders are the overview's former code, unchanged.

import (
	"context"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AutonomyProjectETA is one project's autonomy state for the
// landing page. Provides "when is this project's lead going to fire
// again" so the operator can predict spend bursts and sanity-check
// that autonomy is healthy.
type AutonomyProjectETA struct {
	ProjectID  string
	Interval   time.Duration
	LastEvalAt time.Time
	NextEvalAt time.Time
	// LastOutcome is the most recent evaluation's outcome string
	// (CREATED / NO_ACTION / RATE_LIMITED / ...). Lets operators
	// glance at the page and see whether the loop is firing
	// productively or just churning.
	LastOutcome string
	// AgoLabel is a pre-rendered "3m 24s ago" string for LastEvalAt.
	// Computed handler-side because Go templates can't subtract
	// times cleanly without a helper funcmap entry.
	AgoLabel string
	// EtaLabel is "in 1m 36s" or "due now" for NextEvalAt — the
	// initial server-render value. The dashboard's client-side
	// ticker re-derives this every second from NextEvalAtISO so
	// the countdown is live; without that ticker the label froze
	// at page-load time and never moved (operator-reported
	// 2026-05-05).
	EtaLabel string
	// NextEvalAtISO is NextEvalAt rendered as RFC3339 for the
	// client-side ticker to parse. Zero-time projects emit empty
	// string; the ticker leaves their label alone.
	NextEvalAtISO string
}

// DashboardSpend is the headline $ summary shown on the dashboard.
type DashboardSpend struct {
	Day24hUSD      float64
	Day7USD        float64
	Day30USD       float64
	MonthToDateUSD float64
	// Sparkline is a pre-computed SVG polyline points string for the
	// 24h hourly spend chart. Empty string means "don't render" — repo
	// not wired, no data, or all hours zero.
	Sparkline       string
	SparklineFilled string // closed polyline for the filled-area underlay
}

// loadSpendWindows returns the four rolling spend windows and the 24 h
// sparkline. Instance-wide (SumCost has no project filter), so empty for a
// project-scoped session, as on the overview before.
func (s *Server) loadSpendWindows(ctx context.Context, r *http.Request) DashboardSpend {
	var data struct{ Spend DashboardSpend }
	if s.llmUsageRepo == nil || !requestHasAllProjectAccess(r) {
		return data.Spend
	}
	now := time.Now().UTC()
	windows := []struct {
		setter func(float64)
		since  time.Time
	}{
		{func(v float64) { data.Spend.Day24hUSD = v }, now.Add(-24 * time.Hour)},
		{func(v float64) { data.Spend.Day7USD = v }, now.Add(-7 * 24 * time.Hour)},
		{func(v float64) { data.Spend.Day30USD = v }, now.Add(-30 * 24 * time.Hour)},
		{func(v float64) { data.Spend.MonthToDateUSD = v }, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, w := range windows {
		if v, err := s.llmUsageRepo.SumCost(ctx, w.since, time.Time{}); err == nil {
			w.setter(v)
		}
	}

	// Hourly buckets for the 24h sparkline. We bucket client-side
	// from the raw rows because the repo doesn't expose a bucketed
	// query. PageSize=5000 caps per-request cost — in busy
	// deployments older hours within the window may be undercounted,
	// which is acceptable for a sparkline (the headline number above
	// uses SumCost so it's still accurate).
	since24 := now.Add(-24 * time.Hour)
	usage, err := s.llmUsageRepo.List(ctx, persistence.TaskLLMUsageFilter{
		Since:    &since24,
		PageSize: 5000,
	})
	if err == nil {
		data.Spend.Sparkline, data.Spend.SparklineFilled = sparklinePoints(bucketHourly(usage, now), 100, 24)
	} else {
		s.logger.Warn().Err(err).Msg("failed to load hourly usage for sparkline")
	}
	return data.Spend
}

// loadAutonomyETAs returns each autonomy-enabled project's next evaluation,
// soonest first; a project with no evaluation yet sorts last.
func (s *Server) loadAutonomyETAs(ctx context.Context) []AutonomyProjectETA {
	if s.projectReg == nil || s.autonomyEvalRepo == nil {
		return nil
	}
	now := time.Now().UTC()
	// Fetch the latest eval for every project in one query instead of a
	// List(PageSize:1) per project (E2, audit 2026-07-03).
	latestByProject, _ := s.autonomyEvalRepo.LatestByProject(ctx)
	var etas []AutonomyProjectETA
	for _, p := range s.projectReg.ListProjects() {
		if p == nil || !p.Autonomy.Enabled {
			continue
		}
		etas = append(etas, autonomyETA(p.ID, p.Autonomy.PollInterval, latestByProject[p.ID], now))
	}
	sortAutonomyETAs(etas)
	return etas
}

// sortAutonomyETAs orders soonest first; zero time (no eval yet) last; equal
// keys keep the registry order.
func sortAutonomyETAs(etas []AutonomyProjectETA) {
	sort.SliceStable(etas, func(i, j int) bool {
		a, b := etas[i].NextEvalAt, etas[j].NextEvalAt
		if a.IsZero() || b.IsZero() {
			return !a.IsZero() && b.IsZero()
		}
		return a.Before(b)
	})
}

// autonomyETA projects one project's next evaluation from its last one and
// its poll interval (default 5 minutes).
func autonomyETA(projectID, pollInterval string, last *persistence.AutonomyEvaluation, now time.Time) AutonomyProjectETA {
	interval := 5 * time.Minute
	if pollInterval != "" {
		if d, err := time.ParseDuration(pollInterval); err == nil && d > 0 {
			interval = d
		}
	}
	eta := AutonomyProjectETA{ProjectID: projectID, Interval: interval}
	if last == nil {
		// No evaluation yet — the loop will tick within `interval` of
		// daemon start. We don't know start time here; show the interval as
		// the ETA so the operator sees "soon".
		eta.EtaLabel = "≤ " + humanizeAgo(interval)
		return eta
	}
	eta.LastEvalAt = last.CreatedAt
	eta.LastOutcome = last.Outcome
	eta.NextEvalAt = last.CreatedAt.Add(interval)
	eta.AgoLabel = humanizeAgo(now.Sub(last.CreatedAt))
	eta.EtaLabel = humanizeEta(eta.NextEvalAt.Sub(now))
	eta.NextEvalAtISO = eta.NextEvalAt.UTC().Format(time.RFC3339)
	return eta
}

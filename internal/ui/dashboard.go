// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// overviewNeedsYouRows is how many of the inbox's rows the overview shows;
// the rest are one link away (GitHub #603).
const overviewNeedsYouRows = 5

// DashboardData holds data for the overview (/ui/). It answers, in order:
// does anything need me now, and is anything unhealthy (web UI design, "The
// overview answers 'does anything need me now?' first", 2026-11-07, GitHub
// #603). History lives on the detail pages.
type DashboardData struct {
	Title       string
	CurrentPage string

	// NeedsYou is the first overviewNeedsYouRows of the inbox's rows, built by
	// inboxData, so they carry the inbox's actions.
	NeedsYou []inboxItem
	// NeedsYouCount is the inbox's Count: the nav badge and the length of the
	// list "See all" links to.
	NeedsYouCount int
	// NeedsYouHidden is how many inbox rows the overview does not show.
	NeedsYouHidden int
	// WebWrites, BrokerActions and AgentApprovals count the inbox's other
	// waiting cards; the overview links to them rather than repeating them.
	WebWrites            int
	BrokerActions        int
	AgentApprovals       int
	AgentApprovalsFailed bool

	// Doctor is the last doctor report's error checks, when one has run.
	Doctor DashboardDoctor

	// Health problems only: a healthy state renders nothing here.
	SpendSpike      bool
	SpendSpikeRatio float64
	NotReady        []AdminReadinessCheck

	// AllClear is true when nothing needs the operator and nothing is
	// unhealthy; it is the only thing a healthy overview shows.
	AllClear bool

	// At a glance (§D, operator correction 2026-11-07): the statistics tiles,
	// below Needs you and Health. Filled by loadGlance.
	ProjectCount int
	TaskCounts   map[persistence.TaskStatus]int64
	// 2026.4.11+ financial + model-performance summary. Zero values
	// when task_llm_usage is empty or the repo isn't wired (old deployments).
	Spend        DashboardSpend
	WindowDays   int                          // active leaderboard window: 1, 7, or 30
	WindowLabel  string                       // operator-facing label: "1 day", "7 days", "30 days"
	WindowParam  string                       // query value: "1d", "7d", or "30d"
	TopModels1d  []persistence.RoleModelSpend // top 10 by 1-day cost
	TopModels7d  []persistence.RoleModelSpend // top 10 by 7-day cost
	TopModels30d []persistence.RoleModelSpend // top 10 by 30-day cost

	// Landing-page tiles (2026-04-30). Pulls in-flight signals into
	// one front door so operators don't have to bookmark per-page
	// URLs to see "what's happening right now".
	ActiveTasks     []*persistence.Task // status in QUEUED/LEASED/RUNNING, capped at Limit
	ActiveTaskCount int
	ActiveChatCount int                  // count of active Telegram conversations
	AutonomyETAs    []AutonomyProjectETA // per-project next-eval projection, sorted by soonest
	// Limit / LimitOptions drive the shared page-size selector on
	// the landing page's Active Tasks tile. Defaults to 20 like the
	// audit page; the operator picks from 10/20/50/100 via ?limit=N.
	Limit        int
	LimitOptions []int

	// 2026-05-12: surface newer subsystems on the landing page so the
	// operator doesn't have to drill into /ui/memory or per-project
	// pages to see "is the pipeline healthy / are there safety events
	// / how's the judge doing". All three structs zero-value cleanly
	// when the underlying repos aren't wired.
	Memory   DashboardMemory   // KG extraction + ingest queue snapshot
	Judge    DashboardJudge    // hallucination judge rollup over 24h
	Learning DashboardLearning // skill-store maturity + instinct rollup

	// 2026-05-22: cache effectiveness tile so the operator sees
	// Phase E paying off from the front door. Available=false hides
	// the tile entirely on installs where the cache is disabled or
	// has no hits yet — avoids a confusing "$0 saved" empty state.
	CacheSavings DashboardCacheSavings

	// GlanceRestricted: a project-scoped session; the instance-wide tiles
	// stay empty and the page says why.
	GlanceRestricted bool

	// The home page's own blocks (#756, design §14.2), filled by loadHome.
	Greeting       string
	StatusLine     string
	DoneToday      int64
	DoneTodayShown bool
	NextScheduled  *AutonomyProjectETA
	RecentDone     []homeRecent

	// IsSession is true when the request authenticated via a browser
	// session cookie (github-login phase 3). Drives the nav logout
	// button — api-key callers have no session to revoke. Read by the
	// hasSessionFlag template helper.
	IsSession bool
}

// NavAttentionCount implements navAttentionCounter: the overview sets the nav
// badge to the inbox's count, as the inbox page does.
func (d DashboardData) NavAttentionCount() int { return d.NeedsYouCount }

// DashboardDoctor is the overview's doctor line: the names of the error checks
// in the last report any caller ran, and its age. Names only: a check's
// message is for the operator's terminal.
type DashboardDoctor struct {
	Shown  bool
	Errors []string
	Age    string
}

// Dashboard renders the overview.
func (s *Server) Dashboard(w http.ResponseWriter, r *http.Request) {
	// Fresh-install stays ahead of BOTH branches below (design
	// outcome-inbox-design.md §5.7: "the fresh-install /ui/setup redirect
	// stays ahead of both") — checked first so a not-yet-configured
	// instance always reaches the setup wizard, even in the edge case of
	// a RoleUser session existing before setup completes.
	if r.URL.Path == "/" && s.onboardingDetector.Config != nil && s.setupStatus(r).FreshInstall {
		http.Redirect(w, r, "/ui/setup", http.StatusFound)
		return
	}
	// GitHub #560 (M1b B2b): tenant_admin passes this check on purpose: the owner gets the admin's dashboard.
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		// Project-scoped users get the operator dashboard's instance-wide
		// aggregates gated away, so land them on the Outcome Inbox (task
		// 4.4, design §5.7) — their day-to-day "what needs me / what did
		// I ask for" surface — rather than an empty dashboard or the
		// Projects list. Previously /ui/tasks; the inbox supersedes it as
		// the non-admin default home.
		http.Redirect(w, r, "/ui/inbox", http.StatusFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	data := DashboardData{
		Title:       "Home",
		CurrentPage: "dashboard",
		IsSession:   requestIsSession(r),
	}

	// 1. Needs you: the inbox's own rows and count (one implementation).
	inbox := s.inboxData(r, false)
	data.NeedsYouCount = inbox.Count
	data.NeedsYou = inbox.Items
	if len(data.NeedsYou) > overviewNeedsYouRows {
		data.NeedsYouHidden = len(data.NeedsYou) - overviewNeedsYouRows
		data.NeedsYou = data.NeedsYou[:overviewNeedsYouRows]
	}
	data.WebWrites = len(inbox.WebWrites)
	data.BrokerActions = len(inbox.BrokerActions)
	data.AgentApprovals = len(inbox.AgentApprovals)
	data.AgentApprovalsFailed = inbox.AgentApprovalsFailed
	data.Doctor = s.overviewDoctor(time.Now())

	// 2. Health: problems only.
	data.SpendSpike, data.SpendSpikeRatio = s.overviewSpendSpike(ctx, r)
	if s.adminReadiness != nil {
		for _, c := range s.adminReadiness.ReadinessChecks(ctx) {
			if !strings.EqualFold(c.Status, "ok") {
				data.NotReady = append(data.NotReady, c)
			}
		}
	}

	// The badge decides: an informational row (a waiting parent, a retry in
	// flight, a closed approval) alone does not hide "All clear"; the inbox
	// page still lists it (outcome-inbox design §5.2a, GitHub #671).
	nothingWaits := data.NeedsYouCount == 0 && !data.AgentApprovalsFailed
	noDoctorErrors := !data.Doctor.Shown || len(data.Doctor.Errors) == 0
	healthy := !data.SpendSpike && len(data.NotReady) == 0
	data.AllClear = nothingWaits && noDoctorErrors && healthy

	// 3. At a glance: the statistics, below (§D).
	s.loadHome(ctx, r, &data)

	s.render(w, "dashboard.html", data)
}

// overviewDoctor reads the last doctor report's error checks; nothing before
// the first report since start, so the overview never runs doctor itself.
func (s *Server) overviewDoctor(now time.Time) DashboardDoctor {
	if s.lastDoctorErrors == nil {
		return DashboardDoctor{}
	}
	names, at, ok := s.lastDoctorErrors()
	if !ok {
		return DashboardDoctor{}
	}
	return DashboardDoctor{Shown: true, Errors: names, Age: humanizeAgo(now.Sub(at))}
}

// overviewSpendSpike reports whether the last 24 h spend is at least twice the
// 7-day daily average, the overview's one spend signal. The 7-day spend must
// exceed $1, so a fresh install never reads as a spike. Instance-wide, so only
// for all-access sessions.
func (s *Server) overviewSpendSpike(ctx context.Context, r *http.Request) (bool, float64) {
	if s.llmUsageRepo == nil || !requestHasAllProjectAccess(r) {
		return false, 0
	}
	now := time.Now().UTC()
	day, err := s.llmUsageRepo.SumCost(ctx, now.Add(-24*time.Hour), time.Time{})
	if err != nil {
		return false, 0
	}
	week, err := s.llmUsageRepo.SumCost(ctx, now.Add(-7*24*time.Hour), time.Time{})
	if err != nil || week <= 1.0 {
		return false, 0
	}
	if ratio := day / (week / 7.0); ratio >= 2.0 {
		return true, ratio
	}
	return false, 0
}

// humanizeAgo formats a positive duration as "3m 24s ago"-style
// text. Negative or zero returns "just now". Pre-rendered handler-
// side because Go templates can't subtract times without a funcmap
// helper.
func humanizeAgo(d time.Duration) string {
	if d <= 0 {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
}

// humanizeEta formats a future-relative duration. Past or zero
// returns "due now"; positive values render as "in 1m 36s".
func humanizeEta(d time.Duration) string {
	if d <= 0 {
		return "due now"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("in %dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// bucketHourly groups usage rows into 24 hourly buckets ending at `now`.
// Index 0 is the bucket from 23..22 hours ago; index 23 is the most
// recent hour. Rows outside the window are silently dropped.
func bucketHourly(rows []*persistence.TaskLLMUsage, now time.Time) []float64 {
	out := make([]float64, 24)
	cutoff := now.Add(-24 * time.Hour)
	for _, r := range rows {
		if r == nil || r.RecordedAt.Before(cutoff) || r.RecordedAt.After(now) {
			continue
		}
		// Hour offset from now: 0 = current hour, 23 = oldest. Flip so
		// the slice reads left-to-right as time moves forward.
		ago := int(now.Sub(r.RecordedAt).Hours())
		if ago < 0 || ago >= 24 {
			continue
		}
		out[23-ago] += r.CostUSD
	}
	return out
}

// sparklinePoints scales a value series to fit a width × height SVG
// viewBox and returns two SVG `points` strings:
//
//   - line: the polyline through the data points
//   - filled: the same path closed at the bottom corners, for an area fill
//
// Returns "", "" when there's nothing to draw (empty series, single point,
// or all zeros). Computing this in Go avoids fragile template arithmetic.
func sparklinePoints(values []float64, width, height int) (line, filled string) {
	if len(values) < 2 {
		return "", ""
	}
	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return "", ""
	}
	step := float64(width) / float64(len(values)-1)
	var lb, fb strings.Builder
	for i, v := range values {
		x := float64(i) * step
		y := float64(height) - (v/max)*float64(height)
		if i > 0 {
			lb.WriteByte(' ')
			fb.WriteByte(' ')
		}
		fmt.Fprintf(&lb, "%.1f,%.1f", x, y)
		fmt.Fprintf(&fb, "%.1f,%.1f", x, y)
	}
	// Close the filled-area polyline along the baseline so it paints as
	// a filled region instead of a stroke.
	fmt.Fprintf(&fb, " %d,%d 0,%d", width, height, height)
	return lb.String(), fb.String()
}

// DiagnosticsOverview serves /ui/diagnostics: the At-a-glance tiles that
// lived on the home page before #756, moved unchanged (§14.2). Every session
// may open it: loadGlance withholds instance-wide figures from a
// project-scoped session and the page says why. It lives beside Dashboard
// so the session-role read stays in one census-listed file (LLD 18 L2a).
func (s *Server) DiagnosticsOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := DashboardData{
		Title:       "Diagnostics",
		CurrentPage: "diagnostics-overview",
		IsSession:   requestIsSession(r),
	}
	s.loadGlance(ctx, r, &data)
	s.render(w, "diagnostics_overview.html", data)
}

// requestIsSession reports whether the request came with a browser session
// (drives the nav's sign-out control). One read site for both pages in this
// file, so the session-role census (LLD 18 L2a) is unchanged by #756.
func requestIsSession(r *http.Request) bool {
	return api.SessionRoleFromContext(r.Context()) != ""
}

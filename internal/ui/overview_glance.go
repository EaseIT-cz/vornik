package ui

// The overview's "At a glance" section (web UI design, "The overview
// answers...", §D, operator correction 2026-11-07, GitHub #603): the
// statistics tiles return below Needs you and Health, with the data and code
// they had at 052ea9e51, split into one small loader per tile. The spend
// windows and the autonomy list share their loaders with Spend and Projects
// (overview_moved_tiles.go).

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// maxActiveTasksPrealloc caps the pre-allocated capacity of the
// dashboard "active tasks" slice. `limit` is derived from the ?limit=N
// query parameter; parsePageSize whitelists it to the PageSizeOptions
// set (max 100), but that equality-whitelist isn't a recognizable
// upper bound at the allocation site, so the capacity hint is clamped
// here to bound the pre-allocation (CodeQL go/uncontrolled-allocation-size).
const maxActiveTasksPrealloc = 1000

// DashboardMemory is the cross-project memory-pipeline snapshot
// shown on the landing page. KG fields mirror persistence.KGStats
// for the global extraction worker; IngestBacklog is the sum of
// `queued+processing` rows across every project, which spikes
// when ingestion is starved (the broker is publishing faster than
// the worker can drain).
type DashboardMemory struct {
	Enabled        bool // chunkGraph repo wired
	ChunksPending  int
	ChunksDone     int
	ChunksTotal    int
	PercentDone    float64
	Entities       int
	Edges          int
	Mentions       int
	IngestBacklog  int // sum across all projects (queued+processing rows)
	IngestProjects int // number of projects with any backlog
}

// DashboardJudge is the LLM-as-judge verdict rollup shown on the
// landing page. Pulled from task_judge_verdicts over the last 24h
// (or the most recent N rows if the window has no traffic). Zero
// when judgeVerdictRepo isn't wired or the project has no
// verdicts yet.
type DashboardJudge struct {
	Enabled     bool // judgeVerdictRepo wired
	WindowHours int  // window the counts cover (24h by default)
	Pass        int
	Fail        int
	Abstain     int
	Total       int
	PercentPass float64 // Pass / Total * 100, 0 when Total == 0
}

// DashboardLearning is the "Learning" tile — what the deployment has
// distilled from its own runs, and what of that is waiting on the operator.
// It fills the third slot of the "Health & knowledge" row, empty since the
// trading tile was removed (2026-08-07).
//
// The skill half ships in both editions. The instinct half does not:
// instincts are Enterprise-only (every existing entry point sits behind
// .EnterpriseAdmin), so InstinctsAvailable gates that row and a Community
// dashboard simply renders one row fewer — no empty slot, no upsell.
type DashboardLearning struct {
	// Enabled means the counts below are real: skillRepo is wired AND the
	// caller may see instance-wide totals.
	Enabled bool
	// Restricted distinguishes "you may not see this" from "this isn't
	// wired". Both render an empty tile, but conflating them would tell a
	// project-scoped operator their deployment lacks a skill store.
	Restricted bool
	Trusted    int
	Active     int
	Draft      int // == the operator review-queue depth
	Retired    int // counted but not rendered; excluded from Total
	// Total is trusted+active+draft. Retired skills inject nowhere, so
	// including them would overstate what the store contributes.
	Total int

	// InstinctsAvailable is true only in an Enterprise build with the
	// instinct repo wired.
	InstinctsAvailable bool
	// InstinctsLive counts active + promoted. A promoted instinct is still
	// firing, so excluding it would under-report; candidates and retired
	// instincts fire nowhere and are excluded.
	InstinctsLive int
}

// DashboardCacheSavings is the at-a-glance LLM-cache effectiveness
// tile on the landing page. Pulls the response cache's lifetime
// hits + $ saved so operators see the feature paying off without
// drilling into /ui/spend. Available=false hides the tile (cache
// disabled or no hits yet — a "$0 saved" tile is more confusing
// than an absent one on fresh installs).
type DashboardCacheSavings struct {
	Available       bool
	TotalSavingsUSD float64
	TotalHits       int64
}

// loadGlance fills the At a glance section.
func (s *Server) loadGlance(ctx context.Context, r *http.Request, data *DashboardData) {
	glanceWindow(r, data)
	// These gate themselves on all-access or filter by project.
	data.Spend = s.loadSpendWindows(ctx, r)
	s.loadLeaderboards(ctx, r, data)
	s.buildLearningTile(ctx, r, data)
	for _, eta := range s.loadAutonomyETAs(ctx) {
		if api.RequestAllowsProject(r, eta.ProjectID) {
			data.AutonomyETAs = append(data.AutonomyETAs, eta)
		}
	}
	// The rest are instance-wide aggregates with no project filter, so a
	// project-scoped session gets none of them (security review 2026-11-08:
	// the pre-#603 overview loaded them for any session that reached it).
	if !requestHasAllProjectAccess(r) {
		data.GlanceRestricted = true
		return
	}
	if s.projectReg != nil {
		data.ProjectCount = s.projectReg.GetStats().ProjectCount
	}
	if s.taskRepo != nil {
		if counts, err := s.taskRepo.CountByStatus(ctx, ""); err == nil {
			data.TaskCounts = counts
		} else {
			s.logger.Warn().Err(err).Msg("failed to load task counts for dashboard")
		}
	}
	s.loadCacheSavings(ctx, data)
	s.loadActiveTasks(ctx, data)
	if s.activeChatSource != nil {
		data.ActiveChatCount = s.activeChatSource.ActiveChatCount()
	}
	s.loadMemoryTile(ctx, r, data)
	s.loadJudgeTile(ctx, data)
}

// glanceWindow reads ?window= for the leaderboard and ?limit= for the active
// tasks list.
func glanceWindow(r *http.Request, data *DashboardData) {
	data.TaskCounts = make(map[persistence.TaskStatus]int64)
	data.WindowDays, data.WindowLabel, data.WindowParam = 7, "7 days", "7d"
	data.Limit = parsePageSize(r.URL.Query().Get("limit"))
	data.LimitOptions = PageSizeOptions
	switch r.URL.Query().Get("window") {
	case "24h", "1d":
		data.WindowDays = 1
		data.WindowLabel = "1 day"
		data.WindowParam = "1d"
	case "30d":
		data.WindowDays = 30
		data.WindowLabel = "30 days"
		data.WindowParam = "30d"
	case "7d":
		data.WindowDays = 7
		data.WindowLabel = "7 days"
		data.WindowParam = "7d"
	}
}

// loadLeaderboards preloads the three leaderboard windows; instance-wide, so
// all-access sessions only.
func (s *Server) loadLeaderboards(ctx context.Context, r *http.Request, data *DashboardData) {
	if s.llmUsageRepo == nil || !requestHasAllProjectAccess(r) {
		return
	}
	now := time.Now().UTC()
	loadLeaderboard := func(since time.Time, setter func([]persistence.RoleModelSpend), label string) {
		rows, err := s.llmUsageRepo.AggregateByRoleModel(ctx, since, time.Time{}, 10, "")
		if err != nil {
			s.logger.Warn().Err(err).Str("window", label).Msg("failed to load model leaderboard for dashboard")
			return
		}
		// Replace raw role identifiers (kg_extractor, judge, ...)
		// with operator-facing labels for display. The DB rows
		// stay raw — this is a render-side transform only.
		for i := range rows {
			rows[i].Role = displayRole(rows[i].Role)
		}
		setter(rows)
	}
	// Preload all three small result sets. The dashboard switches between
	// them client-side, so the operator gets an instant tab change rather
	// than a full page reload or another round-trip.
	loadLeaderboard(now.Add(-24*time.Hour), func(rows []persistence.RoleModelSpend) {
		data.TopModels1d = rows
	}, "1d")
	loadLeaderboard(now.Add(-7*24*time.Hour), func(rows []persistence.RoleModelSpend) {
		data.TopModels7d = rows
	}, "7d")
	loadLeaderboard(now.Add(-30*24*time.Hour), func(rows []persistence.RoleModelSpend) {
		data.TopModels30d = rows
	}, "30d")
}

// loadCacheSavings fills the response-cache tile (Phase E).
func (s *Server) loadCacheSavings(ctx context.Context, data *DashboardData) {
	// Cache savings tile (Phase E). Tight 2s timeout — the landing
	// page must render even if the cache stats query stalls. Hides
	// itself when no hits yet so a fresh install isn't dominated by
	// a "$0.00 saved" placeholder.
	if s.responseCacheStats != nil {
		csCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stats, err := s.responseCacheStats.CacheStats(csCtx)
		cancel()
		if err == nil && stats.TotalHits > 0 {
			data.CacheSavings = DashboardCacheSavings{
				Available:       true,
				TotalSavingsUSD: stats.TotalSavingsUSD,
				TotalHits:       stats.TotalHits,
			}
		}
	}
}

// loadActiveTasks fills the active-tasks tile.
func (s *Server) loadActiveTasks(ctx context.Context, data *DashboardData) {
	limit := data.Limit
	// Active tasks tile: focused list of in-flight work. Selects
	// QUEUED / LEASED / RUNNING separately and concatenates so a
	// dashboard that shows mostly "queued, queued, running" reads
	// at a glance. Row cap is `limit` (?limit=N — same shared
	// validator as the rest of the UI; default 20). The legacy
	// fixed cap of 8 was raised to the audit-default 20 so the
	// landing page matches the rest of the list views.
	if s.taskRepo != nil {
		active := make([]*persistence.Task, 0, min(limit, maxActiveTasksPrealloc))
		for _, status := range []persistence.TaskStatus{
			persistence.TaskStatusRunning,
			persistence.TaskStatusLeased,
			persistence.TaskStatusQueued,
		} {
			st := status
			rows, err := s.taskRepo.List(ctx, persistence.TaskFilter{
				Status:   &st,
				PageSize: limit,
			})
			if err != nil {
				continue
			}
			for _, t := range rows {
				active = append(active, t)
				if len(active) >= limit {
					break
				}
			}
			if len(active) >= limit {
				break
			}
		}
		data.ActiveTasks = active
		// Total counts come from TaskCounts (already populated above);
		// expose a single-int summary for the tile header.
		data.ActiveTaskCount = int(data.TaskCounts[persistence.TaskStatusRunning] +
			data.TaskCounts[persistence.TaskStatusLeased] +
			data.TaskCounts[persistence.TaskStatusQueued])
	}
}

// loadMemoryTile fills the memory-pipeline tile.
func (s *Server) loadMemoryTile(ctx context.Context, r *http.Request, data *DashboardData) {
	// Memory pipeline tile (2026-05-12). Global KG snapshot + cross-
	// project ingest backlog — INSTANCE-WIDE (Stats has no project
	// filter), so gated to all-access callers; a project-scoped session
	// must not see cross-project totals (same leak fixed on /ui/memory).
	// Nil-safe — the template hides the tile when Enabled is false.
	if s.chunkGraph != nil && requestHasAllProjectAccess(r) {
		if stats, err := s.chunkGraph.Stats(ctx); err == nil && stats != nil {
			data.Memory.Enabled = true
			data.Memory.ChunksPending = stats.ChunksPending
			data.Memory.ChunksDone = stats.ChunksDone
			data.Memory.ChunksTotal = stats.ChunksPending + stats.ChunksDone
			if data.Memory.ChunksTotal > 0 {
				data.Memory.PercentDone = 100.0 * float64(stats.ChunksDone) / float64(data.Memory.ChunksTotal)
			}
			data.Memory.Entities = stats.Entities
			data.Memory.Edges = stats.Edges
			data.Memory.Mentions = stats.Mentions
		} else if err != nil {
			s.logger.Warn().Err(err).Msg("failed to load KG stats for dashboard")
		}
	}
	if s.ingestQueue != nil && s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			if p == nil {
				continue
			}
			if d, err := s.ingestQueue.QueueDepth(ctx, p.ID); err == nil && d > 0 {
				data.Memory.IngestBacklog += d
				data.Memory.IngestProjects++
			}
		}
	}
}

// loadJudgeTile fills the judge tile.
func (s *Server) loadJudgeTile(ctx context.Context, data *DashboardData) {
	// Judge verdict tile (2026-05-12). Pulls the most recent 200
	// verdicts across all projects and counts pass/fail/abstain over
	// the last 24h. The repo doesn't expose a windowed Count, so we
	// filter the row slice in Go — cheap at 200 rows and avoids a
	// schema change for the dashboard.
	if s.judgeVerdictRepo != nil {
		data.Judge.Enabled = true
		data.Judge.WindowHours = 24
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		rows, err := s.judgeVerdictRepo.ListRecent(ctx, "", 200)
		if err == nil {
			for _, v := range rows {
				if v == nil || v.RecordedAt.Before(cutoff) {
					continue
				}
				data.Judge.Total++
				switch strings.ToLower(v.Verdict) {
				case "pass":
					data.Judge.Pass++
				case "fail":
					data.Judge.Fail++
				case "abstain":
					data.Judge.Abstain++
				}
			}
			if data.Judge.Total > 0 {
				data.Judge.PercentPass = 100.0 * float64(data.Judge.Pass) / float64(data.Judge.Total)
			}
		}
	}
}

// buildLearningTile fills the "Learning" tile: skill-store maturity
// distribution, the draft review-queue depth, and (Enterprise only) the count
// of live instincts.
//
// Both counts are instance-wide aggregates with no project filter, so they are
// gated to all-access callers exactly like the spend and KG rollups above — a
// project-scoped session must not read cross-project totals.
func (s *Server) buildLearningTile(ctx context.Context, r *http.Request, data *DashboardData) {
	if s.skillRepo == nil {
		return
	}
	if !requestHasAllProjectAccess(r) {
		data.Learning.Restricted = true
		return
	}

	counts, err := s.skillRepo.CountByMaturity(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to count skills by maturity for dashboard")
		return
	}
	data.Learning.Enabled = true
	data.Learning.Trusted = counts[persistence.SkillMaturityTrusted]
	data.Learning.Active = counts[persistence.SkillMaturityActive]
	data.Learning.Draft = counts[persistence.SkillMaturityDraft]
	data.Learning.Retired = counts[persistence.SkillMaturityRetired]
	data.Learning.Total = data.Learning.Trusted + data.Learning.Active + data.Learning.Draft

	// Instincts are Enterprise-only. enterpriseAdmin (not repo presence) is
	// the edition switch — some EE features have their repo wired in CE too.
	if !s.enterpriseAdmin || s.instinctRepo == nil {
		return
	}
	byDomain, err := s.instinctRepo.CountByDomainStatus(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to count instincts for dashboard")
		return
	}
	data.Learning.InstinctsAvailable = true
	for _, c := range byDomain {
		switch c.Status {
		case persistence.InstinctStatusActive, persistence.InstinctStatusPromoted:
			data.Learning.InstinctsLive += c.Count
		}
	}
}

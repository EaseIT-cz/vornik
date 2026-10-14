package ui

// W8-AD 2026-10-14 — found at: production (operator observation on the
// reference host's web console). /ui/spend read "Total spend · 30 days
// $661.44 — saved $169.49 via prompt cache" while the landing page's
// "LLM cache savings" card read "$ saved (lifetime) $103.52 · LLM calls
// skipped 502108" and drilled down to /ui/spend. Two different caches
// (provider prompt caching over the page window vs Vornik's own response
// cache over every cached entry) shared one name, and a "lifetime" figure
// smaller than a 30-day one read as a contradiction. These tests render
// both pages through the real handlers and templates — the same bytes the
// console serves — which is the closest level to the operator's
// observation a test reaches. See local-llm-response-cache-design.md
// §"Cache-savings figures on the console".

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// roleModelLimitRepo honours AggregateByRoleModel's limit the way both SQL
// drivers do (LIMIT n when n > 0, every row otherwise), so a handler that
// derives a total from a capped leaderboard is caught.
type roleModelLimitRepo struct {
	extendedLLMUsageRepo
	roleModels []persistence.RoleModelSpend
}

func (r *roleModelLimitRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, limit int, _ string) ([]persistence.RoleModelSpend, error) {
	if limit > 0 && len(r.roleModels) > limit {
		return r.roleModels[:limit], nil
	}
	return r.roleModels, nil
}

// cachePricing prices model "m-cache" at $10/M input and $1/M cache read,
// so every 1M cache-read tokens saved $9.
func cachePricing(t *testing.T) *pricing.Table {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pricing.yaml")
	require.NoError(t, os.WriteFile(p, []byte("models:\n  m-cache:\n    input: 10\n    output: 10\n    cache_read: 1\n"), 0o600))
	tbl, err := pricing.Load(p)
	require.NoError(t, err)
	return tbl
}

var tagRE = regexp.MustCompile(`<[^>]*>`)
var spaceRE = regexp.MustCompile(`\s+`)

// visibleText strips tags and collapses whitespace: what a reader sees.
func visibleText(s string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(s, " ")), " "))
}

// section returns the visible text from the first occurrence of marker up to
// n bytes on, for assertions and for the lane report's rendered-text record.
func section(body, marker string, n int) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	end := i + n
	if end > len(body) {
		end = len(body)
	}
	return visibleText(body[i:end])
}

// The operator's figures: response cache $103.52 over 502,108 hits; provider
// prompt-cache savings over 30 days $169.49 (18,832,222 cache-read tokens at
// $9/M saved), i.e. the window figure exceeds the response-cache figure.
func operatorScenario(t *testing.T) (*Server, *stubCacheStatsSource) {
	t.Helper()
	cache := &stubCacheStatsSource{stats: ResponseCacheStats{
		RowCount: 129503, DistinctPurposes: 4, TotalHits: 502108, TotalSavingsUSD: 103.51641973,
	}}
	repo := &roleModelLimitRepo{roleModels: []persistence.RoleModelSpend{
		{Role: "lead", Model: "m-cache", CostUSD: 661.44, StepCount: 900, PromptTokens: 1_000_000, CacheReadTokens: 18_832_222},
	}}
	repo.sum = 661.44
	srv := NewServer(
		WithLLMUsageRepository(repo),
		WithAssistantPricing(cachePricing(t)),
		WithResponseCacheStatsSource(cache),
		WithOnboardingDetector(alreadyOnboardedDetector()),
	)
	return srv, cache
}

func renderSpend(t *testing.T, srv *Server, window string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Spend(rec, httptest.NewRequest(http.MethodGet, "/spend?window="+window, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func renderDashboard(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// TestCacheSavingsFigures_NamedWindowedAndConsistent — the dashboard card
// names the cache it measures, states its window, links to an anchor on
// /ui/spend, and that anchor shows the same numbers under the same name; the
// spend page's prompt-cache figure names the provider prompt cache and the
// page window. Holds in the operator's state where the 30-day prompt-cache
// figure ($169.49) exceeds the response-cache figure ($103.52).
func TestCacheSavingsFigures_NamedWindowedAndConsistent(t *testing.T) {
	srv, _ := operatorScenario(t)

	dash := renderDashboard(t, srv)
	card := section(dash, `id="response-cache-card"`, 3000)
	t.Logf("dashboard card text: %s", card)
	require.NotEmpty(t, card, "dashboard response-cache card must render")
	assert.Contains(t, card, "Response cache (served by Vornik)")
	assert.Contains(t, card, "$103.52")
	assert.Contains(t, card, "502108")
	assert.Contains(t, card, "all cached entries", "card must state its window")
	assert.Contains(t, card, "provider prompt-cache savings", "card must say it is not the prompt-cache figure")
	assert.Contains(t, dash, `href="/ui/spend#response-cache"`, "drill-down must land on the response-cache tile")
	assert.NotContains(t, strings.ToLower(card), "lifetime",
		"the figure is not lifetime (retention evicts cold rows) and 'lifetime' beside a number smaller than a 30-day figure misleads")

	for _, w := range []struct{ q, label string }{{"7d", "7 days"}, {"30d", "30 days"}} {
		page := renderSpend(t, srv, w.q)
		head := section(page, "Total spend ·", 1200)
		tile := section(page, `id="response-cache"`, 4000)
		t.Logf("/ui/spend?window=%s spend tile: %s", w.q, head)
		t.Logf("/ui/spend?window=%s response-cache tile: %s", w.q, tile)

		assert.Contains(t, head, "Total spend · "+w.label)
		assert.Contains(t, head, "saved $169.49 via provider prompt cache · "+w.label)

		require.NotEmpty(t, tile, "drill-down target id=response-cache must exist on /ui/spend")
		assert.Contains(t, tile, "Response cache (served by Vornik)")
		assert.Contains(t, tile, "$103.52", "the card's $ figure must appear on its drill-down target")
		assert.Contains(t, tile, "502108", "the card's call count must appear on its drill-down target")
		assert.Contains(t, tile, "all cached entries")
		assert.Contains(t, tile, "not limited to the page window ("+w.label+")", "tile must say it ignores the page window")
		assert.NotContains(t, strings.ToLower(tile), "lifetime")
	}
}

// TestSpend_PromptCacheSavingsCoverEveryRoleModel — W8-AD 2026-10-14, Bug B1:
// the "saved via prompt cache" figure beside "Total spend" was summed over the
// role/model leaderboard, which is capped at 20 rows; on the reference host
// both the 7d and 30d leaderboards were full (20 rows), so the figure omitted
// every smaller (role, model) pair while the spend beside it did not.
func TestSpend_PromptCacheSavingsCoverEveryRoleModel(t *testing.T) {
	var rows []persistence.RoleModelSpend
	for i := 0; i < 25; i++ {
		rows = append(rows, persistence.RoleModelSpend{
			Role: "r" + string(rune('a'+i)), Model: "m-cache",
			CostUSD: float64(100 - i), StepCount: 1, PromptTokens: 1_000_000, CacheReadTokens: 1_000_000,
		})
	}
	repo := &roleModelLimitRepo{roleModels: rows}
	repo.sum = 2000
	srv := NewServer(WithLLMUsageRepository(repo), WithAssistantPricing(cachePricing(t)))
	page := renderSpend(t, srv, "7d")
	head := section(page, "Total spend ·", 1200)
	// 25 pairs × 1M cache-read tokens × $9/M = $225.00; the capped sum was $180.00.
	assert.Contains(t, head, "saved $225.00 via provider prompt cache", "got: %s", head)
	// The leaderboard itself keeps its display cap.
	start := strings.Index(page, "Top role + model combos")
	require.GreaterOrEqual(t, start, 0, "leaderboard must render")
	board := page[start:]
	end := strings.Index(board, "</table>")
	require.GreaterOrEqual(t, end, 0, "leaderboard table must close")
	board = board[:end]
	assert.Equal(t, 20, strings.Count(board, "hover:bg-dark-700/40"), "leaderboard display cap stays 20")
}

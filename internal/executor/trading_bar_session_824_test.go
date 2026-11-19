package executor

// GitHub #824 (trading entry-policy design, Amendment 2026-11-18 (GitHub
// #824)): the IBKR sidecar stamps a daily bar at midnight UTC on its own
// session date; the executor dated it by the New York clock, so every bar
// landed one day early. Found at: production (SPY stored under Sunday
// 2026-11-15, no Friday 11-13). Regression test at: integration (the
// executor's writer over a real SQLite benchmark repository, bars in the
// sidecar's exact shape). Lower because: the e2e lane has no IBKR gateway.

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// sidecarBar is a daily bar as the IBKR sidecar stamps it: <date>T00:00:00Z.
func sidecarBar(t *testing.T, date string, closePx float64) barClose {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, date+"T00:00:00Z")
	require.NoError(t, err)
	return barClose{TS: ts, High: closePx + 1, Low: closePx - 1, Close: closePx}
}

func TestBenchmarkCloses_SessionIsTheBarsOwnDate_824(t *testing.T) {
	db := sqlitetest.Memory(t)
	repo := sqlite.NewBenchmarkCloseRepository(db.DB)
	e := &Executor{tradingBenchmarkRepo: repo, logger: zerolog.Nop()}
	bars := []barClose{
		sidecarBar(t, "2026-11-12", 756.27),
		sidecarBar(t, "2026-11-13", 748.66),
		sidecarBar(t, "2026-11-16", 738.32),
	}
	// Fetched on Sunday 2026-11-15, as the issue's Friday bar was.
	ny, _ := time.LoadLocation("America/New_York")
	sunday := time.Date(2026, 11, 15, 12, 0, 0, 0, ny)
	e.recordBenchmarkCloses(context.Background(), []string{"SPY"}, "SPY", bars[:2], sunday)
	// And Monday after the close.
	e.recordBenchmarkCloses(context.Background(), []string{"SPY"}, "SPY", bars, time.Date(2026, 11, 16, 17, 0, 0, 0, ny))

	got, err := repo.Range(context.Background(), "SPY", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	stored := map[string]float64{}
	for _, c := range got {
		stored[c.Session.Format("2006-01-02")] = c.Close
	}
	assert.Equal(t, map[string]float64{"2026-11-12": 756.27, "2026-11-13": 748.66, "2026-11-16": 738.32}, stored,
		"each close under its own session; nothing under Sunday 11-15")
}

func TestCompletedBars_TodaysBarIsNotCompletedBeforeTheClose_824(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	bars := []barClose{sidecarBar(t, "2026-11-17", 740), sidecarBar(t, "2026-11-18", 735)}
	intraday := completedBars(bars, time.Date(2026, 11, 18, 11, 0, 0, 0, ny))
	require.Len(t, intraday, 1, "before 16:00 ET today's bar is still in progress")
	assert.Equal(t, 740.0, intraday[0].Close)
	assert.Len(t, completedBars(bars, time.Date(2026, 11, 18, 16, 30, 0, 0, ny)), 2, "after the close it is completed")
}

// The funnel baseline picks the bar of CompletedThrough(observed_at) when
// the bars carry the sidecar's stamp: #719's case, restamped. Before the
// fix the bar labelled 11-09 was 11-10's (close 105), so ret_1 was
// 102/105-1, measured from a price set after the tick.
func TestFunnelOutcomes_BaselineWithSidecarStamps_824(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	repo := &outcomeRepo{pending: []persistence.FunnelCandidate{
		{ExecutionID: "e1", Symbol: "HDB", Intent: "open", ObservedAt: ny(11, 10, 11), RefClose: f(100)},
	}}
	e := &Executor{logger: zerolog.Nop(), tradingFunnelRepo: repo}
	bars := flatSessionsThrough(ny(11, 9, 0), 15) // H 102, L 100, C 101
	bars = append(bars,
		barClose{TS: ny(11, 10, 0), High: 106, Low: 101, Close: 105},
		barClose{TS: ny(11, 11, 0), High: 104, Low: 100, Close: 102},
	)
	for i := range bars { // <date>T00:00:00Z, as the sidecar stamps a daily bar
		d := bars[i].TS
		bars[i].TS = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	}
	e.fillFunnelOutcomes(context.Background(), "trade", "HDB", bars, ny(11, 11, 17))

	require.Contains(t, repo.set, "e1/0")
	assert.InDelta(t, 102.0/101-1, repo.set["e1/0"][0], 1e-9, "ret_1 from the close of 11-09, not 11-10's")
}

// A session that is not a US trading day is never stored.
func TestBenchmarkCloses_RefuseANonTradingDay_824(t *testing.T) {
	db := sqlitetest.Memory(t)
	repo := sqlite.NewBenchmarkCloseRepository(db.DB)
	e := &Executor{tradingBenchmarkRepo: repo, logger: zerolog.Nop()}
	ny, _ := time.LoadLocation("America/New_York")
	bars := []barClose{
		sidecarBar(t, "2026-11-14", 700), // a Saturday
		sidecarBar(t, "2026-11-26", 701), // Thanksgiving
		sidecarBar(t, "2026-11-27", 702), // the Friday after: a trading day
	}
	e.recordBenchmarkCloses(context.Background(), []string{"SPY"}, "SPY", bars, time.Date(2026, 11, 30, 17, 0, 0, 0, ny))
	got, err := repo.Range(context.Background(), "SPY", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "2026-11-27", got[0].Session.Format("2006-01-02"))
}

package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunBenchmarkCloseSuite exercises a BenchmarkCloseRepository (GitHub #597)
// on whichever backend the caller wires.
func RunBenchmarkCloseSuite(t *testing.T, repo persistence.BenchmarkCloseRepository) {
	t.Helper()
	ctx := context.Background()
	d := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	if err := repo.Record(ctx, []persistence.BenchmarkClose{
		{Symbol: "SPY", Session: d(29), Close: 580.5},
		{Symbol: "SPY", Session: d(30), Close: 583},
		{Symbol: "QQQ", Session: d(30), Close: 500},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, []persistence.BenchmarkClose{{Symbol: "SPY", Session: d(30), Close: 1}}); err != nil {
		t.Fatalf("a repeat session is not an error: %v", err)
	}
	got, err := repo.Range(ctx, "SPY", d(29), d(30))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Session.Equal(d(29)) || got[0].Close != 580.5 || got[1].Close != 583 {
		t.Fatalf("first write wins, oldest first, one symbol: %+v", got)
	}
	got, _ = repo.Range(ctx, "SPY", d(30), d(30))
	if len(got) != 1 {
		t.Fatalf("an inclusive one-day range: %+v", got)
	}
}

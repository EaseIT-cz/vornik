package repotest

import (
	"context"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunCompanionHintSuite exercises a CompanionHintRepository (GitHub #634
// slice 2; companion hardening design §13.10) on whichever backend the
// caller wires.
func RunCompanionHintSuite(t *testing.T, repo persistence.CompanionHintRepository) {
	t.Helper()
	t.Run("apply_creates_then_updates_with_the_rule", func(t *testing.T) { hintApplyRule(t, repo) })
	t.Run("windows_are_per_project", func(t *testing.T) { hintWindowsPerProject(t, repo) })
	// Parallel uploads for one hint lose no event (the compare-and-set).
	t.Run("concurrent_apply_loses_nothing", func(t *testing.T) { hintConcurrentApply(t, repo) })
	t.Run("clients_touch_and_list", func(t *testing.T) { hintClients(t, repo) })
}

var hintSuiteNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func hintApplyRule(t *testing.T, repo persistence.CompanionHintRepository) {
	t.Helper()
	ctx := context.Background()
	now := hintSuiteNow
	f := []bool{false}
	w, n, err := repo.Apply(ctx, "p1", "commit.review", []bool{true, false}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("applied = %d, want 2", n)
	}
	if w.Window != "10" || w.Rev != 1 || w.FiredTotal != 2 || w.FollowedTotal != 1 {
		t.Fatalf("first apply: %+v", w)
	}
	for i := 0; i < 8; i++ {
		if w, _, err = repo.Apply(ctx, "p1", "commit.review", f, now); err != nil {
			t.Fatal(err)
		}
	}
	if w.BackoffUntil == nil || !w.BackoffUntil.Equal(now.Add(persistence.HintBackoff)) {
		t.Fatalf("1 of 10 followed backs off, stored: %+v", w)
	}
	if _, n, err := repo.Apply(ctx, "p1", "commit.review", []bool{true}, now); err != nil || n != 0 {
		t.Fatalf("events during a back-off are not applied: n=%d err=%v", n, err)
	}
	got, err := repo.Windows(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	row, ok := got["commit.review"]
	if !ok || row.Window != "1000000000" || row.BackoffUntil == nil || !row.BackoffUntil.Equal(*w.BackoffUntil) || row.Rev != 10 {
		t.Fatalf("read back: %+v", got)
	}
}

func hintWindowsPerProject(t *testing.T, repo persistence.CompanionHintRepository) {
	t.Helper()
	ctx := context.Background()
	now := hintSuiteNow
	if _, _, err := repo.Apply(ctx, "p2", "commit.review", []bool{true}, now); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Windows(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["commit.review"].Window != "1" {
		t.Fatalf("p2 sees only its own row: %+v", got)
	}
	none, err := repo.Windows(ctx, "p-none")
	if err != nil || len(none) != 0 {
		t.Fatalf("an unknown project has no rows: %v %+v", err, none)
	}
}

func hintConcurrentApply(t *testing.T, repo persistence.CompanionHintRepository) {
	t.Helper()
	ctx := context.Background()
	now := hintSuiteNow
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := repo.Apply(ctx, "p3", "test_failed.recall", []bool{true}, now)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("4 writers are within the retry bound: %v", err)
		}
	}
	got, err := repo.Windows(ctx, "p3")
	if err != nil {
		t.Fatal(err)
	}
	if row := got["test_failed.recall"]; row.FiredTotal != 4 || row.Window != "1111" || row.Rev != 4 {
		t.Fatalf("every apply is counted once: %+v", row)
	}
}

func hintClients(t *testing.T, repo persistence.CompanionHintRepository) {
	t.Helper()
	ctx := context.Background()
	now := hintSuiteNow
	if err := repo.TouchFetch(ctx, "k1", "p1", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.TouchUpload(ctx, "k1", "p1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.TouchFetch(ctx, "k1", "p1", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.TouchFetch(ctx, "k2", "p2", now); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Clients(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].KeyID != "k1" || got[0].LastFetchAt == nil || !got[0].LastFetchAt.Equal(now.Add(2*time.Minute)) ||
		got[0].LastUploadAt == nil || !got[0].LastUploadAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("p1's one client, latest times: %+v", got)
	}
}

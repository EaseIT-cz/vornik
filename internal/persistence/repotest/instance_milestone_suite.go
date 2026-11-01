package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunInstanceMilestoneSuite pins the instance's once-only milestones on BOTH
// backends (GitHub #587, operations-api-contract-design.md §7.2.2): the first
// mark of a name is kept, a later mark never moves it, and the marks read
// back as they were written.
func RunInstanceMilestoneSuite(t *testing.T, repo persistence.InstanceMilestoneRepository) {
	ctx := context.Background()

	t.Run("a first mark reads back its time", func(t *testing.T) {
		name := uniqueID("m-")
		at := time.Now().UTC().Truncate(time.Millisecond)
		first, err := repo.MarkMilestone(ctx, name, at)
		wantOK(t, "MarkMilestone", err)
		if !first {
			t.Fatal("the first mark of a name reported it was already marked")
		}
		got, err := repo.Milestones(ctx)
		wantOK(t, "Milestones", err)
		if !got[name].Equal(at) {
			t.Fatalf("Milestones()[%s] = %v; want %v", name, got[name], at)
		}
	})

	t.Run("a second mark keeps the first time", func(t *testing.T) {
		name := uniqueID("m-")
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		_, err := repo.MarkMilestone(ctx, name, at)
		wantOK(t, "MarkMilestone", err)
		first, err := repo.MarkMilestone(ctx, name, time.Now().UTC())
		wantOK(t, "second MarkMilestone", err)
		if first {
			t.Fatal("a second mark of a name reported it as the first")
		}
		got, err := repo.Milestones(ctx)
		wantOK(t, "Milestones", err)
		if !got[name].Equal(at) {
			t.Fatalf("the milestone moved to %v; want %v (a first value happens once)", got[name], at)
		}
	})

	t.Run("an unmarked name is absent", func(t *testing.T) {
		got, err := repo.Milestones(ctx)
		wantOK(t, "Milestones", err)
		if _, ok := got[uniqueID("m-")]; ok {
			t.Fatal("an unmarked name read as marked")
		}
	})
}

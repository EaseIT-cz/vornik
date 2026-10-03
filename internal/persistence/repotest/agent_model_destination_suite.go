package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAgentModelDestinationSuite pins agent_model_provider_approvals on both
// drivers (agent-administered design §18.6 item 2 in detail, round 2 F3 and
// F7, round 3 F3): one row per (namespace, destination); a removal is soft
// (removed_at, history kept); a re-grant clears it; a namespace reads only
// its own rows. Namespaces are run-unique because the Postgres lane's
// database is shared.
func RunAgentModelDestinationSuite(t *testing.T, repo persistence.AgentGrantRepository) {
	t.Helper()
	ctx := context.Background()
	suffix := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("m")))
	ns, other := "m"+uniqueTail(suffix), "n"+uniqueTail(suffix)
	now := time.Now().UTC().Truncate(time.Second)
	const dest = "vertex@aiplatform.googleapis.com"

	t.Run("MissContract", func(t *testing.T) {
		AssertMiss(t, "AgentGrantRepository.GetModelDestination", func() (*persistence.AgentModelDestinationApproval, error) {
			return repo.GetModelDestination(ctx, ns, "absent@nowhere.example")
		})
	})
	t.Run("Grant_soft_remove_regrant", func(t *testing.T) {
		a := persistence.AgentModelDestinationApproval{Namespace: ns, Destination: dest, ApprovedByDevice: "dev_a", ApprovedAt: now}
		if err := repo.UpsertModelDestination(ctx, a); err != nil {
			t.Fatal(err)
		}
		// The same destination in another namespace is another row.
		b := a
		b.Namespace = other
		if err := repo.UpsertModelDestination(ctx, b); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetModelDestination(ctx, ns, dest)
		if err != nil || got.ApprovedByDevice != "dev_a" || !got.ApprovedAt.Equal(now) || got.RemovedAt != nil {
			t.Fatalf("round trip: %+v, %v", got, err)
		}
		list, err := repo.ListModelDestinations(ctx, ns)
		if err != nil || len(list) != 1 || list[0].Namespace != ns || list[0].Destination != dest {
			t.Fatalf("ListModelDestinations leaks or misses: %+v, %v", list, err)
		}
		// Review 20261003-a525 A4: the removal says whether it removed
		// anything, so the console can say "nothing to withdraw".
		if removed, err := repo.MarkModelDestinationRemoved(ctx, ns, "absent@nowhere.example", now); err != nil || removed {
			t.Fatalf("removing an absent destination: removed=%v, %v", removed, err)
		}
		if removed, err := repo.MarkModelDestinationRemoved(ctx, ns, dest, now.Add(time.Hour)); err != nil || !removed {
			t.Fatalf("removing an approved destination: removed=%v, %v", removed, err)
		}
		if removed, err := repo.MarkModelDestinationRemoved(ctx, ns, dest, now.Add(2*time.Hour)); err != nil || removed {
			t.Fatalf("removing it again: removed=%v, %v", removed, err)
		}
		got, _ = repo.GetModelDestination(ctx, ns, dest)
		if got == nil || got.RemovedAt == nil || got.ApprovedByDevice != "dev_a" {
			t.Fatalf("a removal must keep the row and its history: %+v", got)
		}
		if o, _ := repo.GetModelDestination(ctx, other, dest); o == nil || o.RemovedAt != nil {
			t.Fatalf("a removal reached another namespace: %+v", o)
		}
		a.ApprovedByDevice, a.ApprovedAt = "dev_b", now.Add(2*time.Hour)
		if err := repo.UpsertModelDestination(ctx, a); err != nil {
			t.Fatal(err)
		}
		got, _ = repo.GetModelDestination(ctx, ns, dest)
		if got == nil || got.RemovedAt != nil || got.ApprovedByDevice != "dev_b" || !got.ApprovedAt.Equal(now.Add(2*time.Hour)) {
			t.Fatalf("a re-grant must clear removed_at and record the new device: %+v", got)
		}
	})
}

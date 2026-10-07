package hermes

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// GitHub backlog "Two small hazards" (2026-10-07, T10): the lane's containers
// had fixed names and startContainer ran `podman rm -f <name>` first, so a
// second lane removed the first run's database and model server mid-run.
func TestContainerNamesArePerRun(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(runSuffix) {
		t.Fatalf("runSuffix %q is not 8 lowercase hex characters", runSuffix)
	}
	for _, c := range []struct{ name, prefix, old string }{
		{pgContainerName(), "vornik-e2e-pg-", "vornik-e2e-pg"},
		{llamaContainerName(), "vornik-e2e-llama-", "vornik-e2e-llama"},
	} {
		if !strings.HasPrefix(c.name, c.prefix) || len(c.name) <= len(c.prefix) || c.name == c.old {
			t.Fatalf("container name %q is not per-run (prefix %q)", c.name, c.prefix)
		}
		if !strings.HasSuffix(c.name, "-"+runSuffix) {
			t.Fatalf("container name %q does not end in the run suffix %q", c.name, runSuffix)
		}
	}
	pg1, llama1 := pgContainerName(), llamaContainerName()
	if pg2, llama2 := pgContainerName(), llamaContainerName(); pg1 != pg2 || llama1 != llama2 {
		t.Fatal("names are not stable within a process")
	}
	labels := strings.Join(laneLabels(), " ")
	if !strings.Contains(labels, "vornik-e2e-lane=hermes") || !strings.Contains(labels, "vornik-e2e-run="+runSuffix) {
		t.Fatalf("labels %q do not carry the lane and the run id", labels)
	}
}

func TestReaperNeverTouchesALiveYoungSibling(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	old := now.Add(-reapMaxAge - time.Minute).Unix()
	young := now.Add(-reapMaxAge + time.Minute).Unix()
	edge := now.Add(-reapMaxAge).Unix()
	in := []podmanContainer{
		{ID: "running-young", State: "running", Created: young},
		{ID: "created-young", State: "created", Created: young},
		{ID: "exited-young", State: "exited", Created: young},
		{ID: "running-old", State: "running", Created: old},
		{ID: "created-old", State: "created", Created: old},
		{ID: "running-edge", State: "running", Created: edge},
	}
	got := strings.Join(reapCandidates(in, now), ",")
	if got != "exited-young,running-old,created-old" {
		t.Fatalf("reap set = %q; want only exited, or older than %v (never a younger non-exited container)", got, reapMaxAge)
	}
}

func TestReaperListArgsAreScopedToTheLaneLabel(t *testing.T) {
	joined := strings.Join(reaperListArgs(), " ")
	if !strings.Contains(joined, "label=vornik-e2e-lane=hermes") || strings.Contains(joined, "name=") {
		t.Fatalf("reaper list %q must filter on the lane label, never a name prefix", joined)
	}
	if !strings.Contains(joined, "--format json") {
		t.Fatalf("reaper list %q must ask for json (Created is read in Go)", joined)
	}
}

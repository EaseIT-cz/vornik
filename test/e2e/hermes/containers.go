package hermes

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// reapMaxAge bounds how long a lane container may live. The longest lane runs
// about 16 minutes, so a lane container older than this is a leak from a run
// whose t.Cleanup never ran (go test -timeout, SIGKILL, a host crash).
const reapMaxAge = 6 * time.Hour

const (
	laneLabelKey = "vornik-e2e-lane"
	laneLabelVal = "hermes"
	runLabelKey  = "vornik-e2e-run"
)

// runSuffix names this test process's containers. One stack per process (lane
// design, "Containers are named per run"): two stacks in one process would
// share it.
var runSuffix = newRunSuffix()

func newRunSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("hermes e2e: no randomness for the run suffix: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func pgContainerName() string    { return "vornik-e2e-pg-" + runSuffix }
func llamaContainerName() string { return "vornik-e2e-llama-" + runSuffix }

// laneLabels are the podman run arguments that mark a container as this
// lane's and this run's.
func laneLabels() []string {
	return []string{"--label", laneLabelKey + "=" + laneLabelVal, "--label", runLabelKey + "=" + runSuffix}
}

// podmanContainer is the part of `podman ps --format json` the reaper reads.
// Created is Unix seconds.
type podmanContainer struct {
	ID      string
	State   string
	Created int64
}

// reaperListArgs lists the lane's containers by label, never by name prefix.
func reaperListArgs() []string {
	return []string{"ps", "-a", "--filter", "label=" + laneLabelKey + "=" + laneLabelVal, "--format", "json"}
}

// reapCandidates picks the lane containers to remove: those that have exited,
// and any older than reapMaxAge. A container younger than reapMaxAge that has
// not exited (a live sibling run, or one still starting) is never returned.
// podman ps has no "created before" filter, so the cutoff is computed here.
func reapCandidates(in []podmanContainer, now time.Time) []string {
	cutoff := now.Add(-reapMaxAge).Unix()
	var ids []string
	for _, c := range in {
		if c.State == "exited" || c.Created < cutoff {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

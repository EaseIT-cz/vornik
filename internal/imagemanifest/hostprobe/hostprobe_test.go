package hostprobe

import (
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

var _ imagemanifest.Prober = HostProber{}

// TestStackMatchesComposeLabel is the regression test for a defect caught
// before it shipped (2026-08-25).
//
// The first implementation resolved `compose:trading` by filtering containers
// on `io.podman.compose.project=trading`. On a real host that label's value is
// derived from the compose file's DIRECTORY, so every stack under
// deployments/podman/ carries `io.podman.compose.project=podman` — verified
// against a live trading stack. The filter therefore matched nothing, the
// condition never fired, and every broker image would have been silently
// skipped by every update: the exact bug this whole change exists to fix,
// reproduced inside the fix.
//
// The reliable discriminator is which compose FILE the stack was created from.
func TestStackMatchesComposeLabel(t *testing.T) {
	// Values as podman actually reports them: sometimes a path, sometimes a
	// bare filename, and empty for containers not created by compose.
	live := []string{
		"",
		"deployments/podman/trading.compose.yaml",
		"pagedrop.compose.yaml",
	}

	if !stackMatchesConfigFiles(live, "trading") {
		t.Error("a container created from trading.compose.yaml must match the trading stack")
	}
	if !stackMatchesConfigFiles(live, "pagedrop") {
		t.Error("a bare filename must match too — podman reports the label both ways")
	}
	if stackMatchesConfigFiles(live, "cluster") {
		t.Error("no container came from cluster.compose.yaml, so it must not match")
	}
	if stackMatchesConfigFiles(nil, "trading") {
		t.Error("no containers at all must not match")
	}
}

// TestStackMatchIsNotASubstringMatch guards against the lazy fix: plain
// strings.Contains(value, stack) would make `compose:cluster` match
// "supercluster.compose.yaml", and would make a stack named "trade" match
// "trading.compose.yaml".
func TestStackMatchIsNotASubstringMatch(t *testing.T) {
	if stackMatchesConfigFiles([]string{"supercluster.compose.yaml"}, "cluster") {
		t.Error("`cluster` must not match supercluster.compose.yaml")
	}
	if stackMatchesConfigFiles([]string{"trading.compose.yaml"}, "trade") {
		t.Error("`trade` must not match trading.compose.yaml")
	}
}

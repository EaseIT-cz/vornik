package agentbench

// GitHub #790 (agent benchmark design, amendment #790): a release-gate run
// refuses a published task set. A set is unreleased when it sits under a
// tasksets/unreleased/ directory, which the Community export removes.

import (
	"path/filepath"
	"testing"
)

func TestIsUnreleasedTaskSet790(t *testing.T) {
	root := t.TempDir()
	for path, want := range map[string]bool{
		filepath.Join(root, "internal/agentbench/tasksets/unreleased/dev-swarm-tasks-2027.json"):  true,
		filepath.Join(root, "internal/agentbench/tasksets/unreleased/sub/set.json"):               true,
		filepath.Join(root, "internal/agentbench/tasksets/dev-swarm-tasks-v1.json"):               false,
		filepath.Join(root, "internal/agentbench/tasksets/unreleased/../dev-swarm-tasks-v1.json"): false,
		filepath.Join(root, "elsewhere/unreleased/set.json"):                                      false,
		filepath.Join(root, "internal/agentbench/tasksets/unreleased-v2/set.json"):                false,
		"": false,
	} {
		if got := IsUnreleasedTaskSet(path); got != want {
			t.Errorf("%q: %v, want %v", path, got, want)
		}
	}
	// A relative path is resolved against the working directory.
	t.Chdir(root)
	if !IsUnreleasedTaskSet("internal/agentbench/tasksets/unreleased/x.json") {
		t.Error("a relative path under tasksets/unreleased/ was not recognised")
	}
}

func TestEvaluateReleaseGate790_SaysWhenASetWasPublished(t *testing.T) {
	var a, b Journal
	if d := EvaluateReleaseGate(a, b, CalibrationManifest{}, NoiseFloorManifest{}, ReleaseGatePolicy{}); d.TaskSetPublished {
		t.Fatal("no journal was stamped, yet the decision says published")
	}
	b.Manifest.TaskSetPublished = true
	if d := EvaluateReleaseGate(a, b, CalibrationManifest{}, NoiseFloorManifest{}, ReleaseGatePolicy{}); !d.TaskSetPublished {
		t.Fatal("the candidate ran on a published set; the decision does not say so")
	}
}

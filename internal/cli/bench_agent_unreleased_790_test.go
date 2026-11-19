package cli

// GitHub #790 (agent benchmark design, amendment #790): a release-gate run
// refuses a published task set unless --published-task-set, which the
// journal records.

import (
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

func TestReleaseTaskSetsUnreleased790(t *testing.T) {
	root := t.TempDir()
	published := filepath.Join(root, "internal/agentbench/tasksets/dev-swarm-tasks-v1.json")
	unreleased := filepath.Join(root, "internal/agentbench/tasksets/unreleased/dev-swarm-tasks-2027.json")

	if err := releaseTaskSetsUnreleased(false, unreleased, ""); err != nil {
		t.Fatalf("an unreleased set was refused: %v", err)
	}
	for _, paths := range [][]string{{published, ""}, {unreleased, published}} {
		err := releaseTaskSetsUnreleased(false, paths...)
		if err == nil || !strings.Contains(err.Error(), "published task set") || !strings.Contains(err.Error(), "--published-task-set") {
			t.Fatalf("%v: err = %v, want a refusal naming the way on", paths, err)
		}
	}
	if err := releaseTaskSetsUnreleased(true, published, published); err != nil {
		t.Fatalf("--published-task-set still refused: %v", err)
	}
}

func TestValidateReleaseRunInputs790_StrayFlagIsRefused(t *testing.T) {
	old := benchAgentPublishedTaskSet
	t.Cleanup(func() { benchAgentPublishedTaskSet = old })
	benchAgentPublishedTaskSet = true
	err := validateReleaseRunInputs(agentbench.PreRegistration{}, agentbench.ArmFields{}, nil)
	if err == nil || !strings.Contains(err.Error(), "--published-task-set") {
		t.Fatalf("a non-release run took --published-task-set: %v", err)
	}
	benchAgentPublishedTaskSet = false
	if err := validateReleaseRunInputs(agentbench.PreRegistration{}, agentbench.ArmFields{}, nil); err != nil {
		t.Fatalf("a non-release run without the flag: %v", err)
	}
}

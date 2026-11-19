package agentbench

// GitHub #260, #790 (agent benchmark design, amendment "the first unreleased
// set is a rework tier"): the first unreleased task set loads, and every task
// is a dev-pipeline exploratory task with one target and an acceptance suite.
// Absent in a Community checkout, where the export removes the directory.

import (
	"encoding/json"
	"os"
	"testing"
)

func TestReworkSet260_LoadsAndIsWellFormed(t *testing.T) {
	const path = "tasksets/unreleased/dev-swarm-rework-2026.json"
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("no unreleased set in this checkout")
	}
	if err != nil {
		t.Fatal(err)
	}
	var tasks []TaskSpec
	if err := json.Unmarshal(raw, &tasks); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTaskTargets(tasks, true); err != nil {
		t.Fatalf("targets: %v", err)
	}
	if !IsUnreleasedTaskSet(path) {
		t.Fatal("the set is not recognised as unreleased (#790)")
	}
	if len(tasks) < 6 {
		t.Fatalf("%d tasks, want at least 6", len(tasks))
	}
	for _, task := range tasks {
		if task.Workflow != "dev-pipeline" || task.Tier != TaskTierExploratory || len(task.Targets) != 1 || task.Acceptance == "" {
			t.Errorf("%s: workflow %q tier %q targets %v acceptance %q", task.ID, task.Workflow, task.Tier, task.Targets, task.Acceptance)
		}
	}
}

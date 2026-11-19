package agentbench

import (
	"strings"
	"testing"
)

// GitHub #808 (benchmark LLD §12.33 item 3): every reset rewrites the
// baseline, so a task whose target lies under it is refused at load.
func TestValidateTaskTargets_RefusesATargetUnderTheBaseline_808(t *testing.T) {
	for _, tg := range []string{"go.mod", ".vornik", ".vornik/notes.md"} {
		task := TaskSpec{ID: "t", Prompt: "write " + tg, Targets: []string{tg}}
		if err := ValidateTaskTargets([]TaskSpec{task}, true); err == nil || !strings.Contains(err.Error(), "baseline") {
			t.Errorf("target %q: %v, want the baseline refusal", tg, err)
		}
	}
	ok := TaskSpec{ID: "t", Prompt: "write scratch/go.mod.d/x and go.modx", Targets: []string{"scratch/go.mod.d", "go.modx"}}
	if err := ValidateTaskTargets([]TaskSpec{ok}, true); err != nil {
		t.Errorf("targets that only share a prefix are refused: %v", err)
	}
}

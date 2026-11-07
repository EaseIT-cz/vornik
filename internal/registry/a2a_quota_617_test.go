package registry

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #617 (federation design, amendment 2026-11-06): a published workflow
// bounds how many tasks one caller key may create per UTC day. Before the
// change there was no quota at all.
func TestWorkflowA2A_DailyQuotaPerKey_617(t *testing.T) {
	var a WorkflowA2A
	if err := yaml.Unmarshal([]byte("publish: true\ndailyQuotaPerKey: 12\n"), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.EffectiveDailyQuotaPerKey(); got != 12 {
		t.Fatalf("set quota: got %d, want 12", got)
	}
	if got := (WorkflowA2A{Publish: true}).EffectiveDailyQuotaPerKey(); got != 50 {
		t.Fatalf("unset quota: got %d, want the default 50", got)
	}

	wf := func(q int) *Workflow {
		return &Workflow{
			ID: "w", Entrypoint: "s",
			Steps:     map[string]WorkflowStep{"s": {Type: "agent", Role: "r", OnSuccess: "done"}},
			Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
			A2A:       WorkflowA2A{Publish: true, DailyQuotaPerKey: q},
		}
	}
	if err := wf(0).Validate("w.md"); err != nil {
		t.Fatalf("an unset quota must load: %v", err)
	}
	if err := wf(-1).Validate("w.md"); err == nil || !strings.Contains(err.Error(), "dailyQuotaPerKey") {
		t.Fatalf("a negative quota must be refused naming the field, got %v", err)
	}
}

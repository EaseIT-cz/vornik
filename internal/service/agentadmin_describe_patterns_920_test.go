package service

import (
	"context"
	"strings"
	"testing"
)

// GitHub #920 (agent-administered design part 24 §24.6 S4): an agent reads
// the five ways to lay out its projects before it builds anything.
func TestAgentAdmin_DescribeStatesTheWorkspacePatterns_920(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Patterns) != 5 {
		t.Fatalf("patterns = %d, want 5: %q", len(caps.Patterns), caps.Patterns)
	}
	all := strings.Join(caps.Patterns, "\n")
	for _, want := range []string{"memory sealed", "shared_memory", "private data", "hand_over", "Your own memory is separate"} {
		if !strings.Contains(all, want) {
			t.Errorf("the patterns do not say %q", want)
		}
	}
	if !strings.Contains(caps.HowToWork, "Lay out your projects with these patterns") {
		t.Error("the guidance does not carry the patterns")
	}
	found := false
	for _, n := range caps.NeedsApproval {
		if strings.Contains(n, "shared memory") {
			found = true
		}
	}
	if !found {
		t.Errorf("needs_approval does not name joining the shared memory: %q", caps.NeedsApproval)
	}
}

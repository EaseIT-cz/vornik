package dispatcher

// GitHub #648 (dispatcher turn-context design §10). Found at: production,
// 2026-11-04: a research request went to vornik-marketing because its name
// matched the topic; the prompt listed projects by name only, not by what
// they can run.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestPromptsListWhatEachProjectCanRun_648(t *testing.T) {
	projects := []*registry.Project{
		{ID: "assistant", DefaultWorkflowID: "adaptive", AdaptiveCandidateWorkflows: []string{"research", "deep-research"}},
		{ID: "vornik-marketing", DefaultWorkflowID: "adaptive", AdaptiveCandidateWorkflows: []string{"moltbook-engage", "outreach-discover"}},
		{ID: "headmatch", DefaultWorkflowID: "dev-pipeline"},
	}
	for name, out := range map[string]string{
		"generic": BuildSystemPrompt("assistant", projects),
		"lead":    BuildLeadSystemPrompt(projects[0], nil, "You are the lead.", projects),
	} {
		if !strings.Contains(out, "can run: moltbook-engage, outreach-discover") {
			t.Fatalf("%s prompt must say what vornik-marketing can run", name)
		}
		if !strings.Contains(out, "can run: dev-pipeline") {
			t.Fatalf("%s prompt must fall back to the default workflow", name)
		}
	}
	if !strings.Contains(BuildLeadSystemPrompt(projects[0], nil, "You are the lead.", projects), "It can run: research, deep-research") {
		t.Fatal("the lead prompt must say what the active project can run (review 40c3 F1)")
	}
	if !strings.Contains(sizeTheWorkBlock, "Choose the project by what it can run") {
		t.Fatal("the sizing rule must say to pick the project by what it can run")
	}
}

func TestProjectCapabilitiesCapped_648(t *testing.T) {
	p := &registry.Project{ID: "x", AdaptiveCandidateWorkflows: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}}
	got := projectCapabilities(p)
	if got != "a, b, c, d, e, f, g, h and 2 more" {
		t.Fatalf("got %q", got)
	}
}

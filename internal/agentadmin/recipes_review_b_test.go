package agentadmin

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Review 20261003-6b46 F1: the gate's error carries the SETUP_INCOMPLETE
// class and names only the credential (and the workflow), never a value.
func TestSetupIncompleteError_ClassAndMessage(t *testing.T) {
	e := &SetupIncompleteError{Workflow: "hermes--personal--agenda", Credential: "CALENDAR_TOKEN"}
	if e.FailureClass() != persistence.TaskFailureClassSetupIncomplete {
		t.Fatalf("class %q", e.FailureClass())
	}
	if got := e.Error(); got != "SETUP_INCOMPLETE: enter CALENDAR_TOKEN on your phone; the workflow hermes--personal--agenda cannot run until it is set" {
		t.Fatalf("message %q", got)
	}
}

// Review 20261003-6b46 F2: a tools-approval refusal for a recipe server
// says which tools are missing as a typed field, so the service records
// the gap without reading the refusal's text.
func TestApproveServerTools_RefusalCarriesTheMissingTools(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail", Tools: []string{"gmail_search"}})
	tr.mustClass(c, Refused)
	if strings.Join(c.MissingTools, ",") != "gmail_get" {
		t.Fatalf("missing tools %v", c.MissingTools)
	}
}

// Review 20261003-6b46 F4: a removal says what kind of narrowing it is, so
// the counter's label follows the change, not the place that counts it.
func TestInstallRecipe_RemovalsAreKindRecipeTools(t *testing.T) {
	tr := probeTree(t)
	tr.apply(tr.render(VerbInstallRecipe, probeInstall(nil)))
	v2 := strings.Replace(strings.Replace(strings.Replace(probeRecipe, "version: 1\nenvelope", "version: 2\nenvelope", 1),
		"read_tools: [look, peek]", "read_tools: [look]", 1), "mcp__src__look, mcp__src__peek]", "mcp__src__look]", 1)
	tr.r = rendererWith(t, "", map[string]string{"probe": v2})
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	if len(c.Narrow.RemovedTools) != 1 || c.Narrow.RemovedTools[0].Kind != NarrowingRecipeTools {
		t.Fatalf("removals %+v", c.Narrow.RemovedTools)
	}
}

package registry

import (
	"os"
	"testing"
)

// GitHub #801: the App review workflow parses, validates, and carries the
// step settings the handlers read.
func TestWorkflowAppReview801_Loads(t *testing.T) {
	b, err := os.ReadFile("../../configs/workflows/github-app-review.md")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ParseWorkflowMarkdown(b, "workflows/github-app-review.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := wf.Validate("workflows/github-app-review.md"); err != nil {
		t.Fatal(err)
	}
	if !wf.Steps["fetch_diff"].MaskSecrets || !wf.Steps["post"].GroundFindings || !wf.Steps["post"].CheckRun || wf.Steps["post"].GatingReviews {
		t.Fatalf("step settings lost: %+v %+v", wf.Steps["fetch_diff"], wf.Steps["post"])
	}
	if g := wf.Steps["context_t"].Gather; len(g) != 2 || g[0] != "hunt_a" {
		t.Fatalf("gather = %v", g)
	}
	if wf.Steps["hunt_b"].Prompt == "" || wf.Steps["hunt_b"].Prompt != wf.Steps["hunt_a"].Prompt {
		t.Fatal("the shared hunter prompt did not resolve")
	}
}

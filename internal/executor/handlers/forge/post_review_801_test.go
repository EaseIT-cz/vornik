package forge

// GitHub #801 C and F at the handler: forge.post_review with
// ground_findings drops an ungrounded finding and counts it, and posts a
// neutral check run whose annotations are the kept findings. Tests at:
// integration (the real handler, a fake provider).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

type checkingProvider struct {
	*fakeProvider
	runs  []forgeapi.CheckRunSpec
	fails int
}

func (c *checkingProvider) CreateCheckRun(_ context.Context, _ string, s forgeapi.CheckRunSpec) error {
	c.runs = append(c.runs, s)
	if c.fails > 0 {
		c.fails--
		return errors.New("boom")
	}
	return nil
}

func TestPostReview801_GroundedCommentAndNeutralCheck(t *testing.T) {
	prov := &checkingProvider{fakeProvider: &fakeProvider{}, fails: 1}
	h := newTestPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithSecretsDetector(detector801(t))
	fetched, _ := json.Marshal(map[string]any{"diff": "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n x\n+y\n",
		"head_sha": "abc123", "not_in_diff": []string{"go.sum"}})
	verdict, _ := json.Marshal(map[string]any{"review": map[string]any{"approved": false, "summary": "One problem.",
		"findings": []map[string]any{
			{"file": "a.go", "line": 2, "severity": "blocker", "title": "nil", "detail": "y nil"},
			{"file": "a.go", "line": 1, "severity": "minor", "title": "ctx", "detail": "context line"},
		},
		// Model text in not_read: a planted key, and a name not in the diff.
		"not_read": []string{"AKIAIOSFODNN7EXAMPLE", "a.go"}}})
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4}), PrevResult: verdict,
		Step:        &registry.WorkflowStep{GroundFindings: true, CheckRun: true},
		StepResults: map[string]json.RawMessage{"fetch_diff": fetched}}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	body := prov.gotReview.Body
	if prov.gotReview.Event != forgeapi.ReviewComment || !strings.Contains(body, "`a.go:2` **nil**") || strings.Contains(body, "context line") ||
		!strings.Contains(body, "dropped 1 findings that cited lines outside the diff") || !strings.Contains(body, "Not reviewed: 2 files (go.sum, a.go)") ||
		strings.Contains(body, "AKIA") {
		t.Fatalf("review %s:\n%s", prov.gotReview.Event, body)
	}
	// GitHub #244: the kept finding is an inline comment on the reviewed head.
	if c := prov.gotReview.Comments; prov.gotReview.CommitID != "abc123" || len(c) != 1 || c[0].Path != "a.go" || c[0].Line != 2 {
		t.Fatalf("inline comments %+v on %q", c, prov.gotReview.CommitID)
	}
	if len(prov.runs) != 2 {
		t.Fatalf("check runs tried %d times, want a retry after one failure", len(prov.runs))
	}
	run := prov.runs[1]
	if run.Conclusion != "neutral" || run.HeadSHA != "abc123" || len(run.Annotations) != 1 || !strings.Contains(run.Summary, "AI") {
		t.Fatalf("check run = %+v", run)
	}
	// Without the fetch_diff result nothing ungrounded is posted.
	in.StepResults = nil
	if _, err := h.Execute(context.Background(), in); err == nil {
		t.Fatal("posted with nothing to ground against")
	}
}

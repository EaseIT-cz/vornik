package executor

// GitHub #751 (reviewable-output design §6.12): a companion review opens with
// its verdict and a numbered action list, or "No action: ship.", within its
// first 150 words. The operator's timed reading sample (§6.11) found one
// review with evidence and no recommendation ("I don't see any
// recommendations, only evidence of problems") and readable reviews that ran
// 950 to 2,100 words before the reader could act.
//
// Found at: production (the operator's timed reading sample, 2026-11-13).
// Regression test at: unit, the step-outcome participant over real files in a
// workspace; the shipped workflows are checked to opt in.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
)

const goodReview751 = `**Verdict: NOT GREEN.** The cache key can hand a wider token to a narrower caller.

## Actions

1. Key the cache on the scope (installation_token_source.go:48).
2. Add a test for a scoped and an unscoped token.

## Findings

- HIGH: the cache keys on the installation only.
`

func TestReviewActionsProblem751(t *testing.T) {
	long := strings.Repeat("evidence ", 160)
	for name, tc := range map[string]struct {
		text, wantIn string
	}{
		"verdict then numbered actions":             {goodReview751, ""},
		"no action needed":                          {"Verdict: GREEN. Nothing blocks it.\n\nNo action: ship.\n\n## Findings\n\nNone.\n", ""},
		"bold verdict (review a233)":                {"**Verdict:** GREEN.\n\n## Actions\n\n1) Merge it.\n", ""},
		"the format quoted, not used (review 6eb1)": {"Verdict: NOT GREEN.\n\n## Findings\n\n- the contract accepts `No action: ship.` but this review has a bug\n", "no action list"},
		"evidence and no action list":               {"Verdict: NOT GREEN.\n\n## Findings\n\n- HIGH: a bug.\n\n## Suggestions\n\n1. fix it\n", "no action list"},
		"an Actions heading, no items":              {"Verdict: NOT GREEN.\n\n## Actions\n\nFix things.\n", "no numbered item"},
		"actions after 150 words":                   {"Verdict: NOT GREEN. " + long + "\n\n## Actions\n\n1. Fix it.\n", "first 150 words"},
		"no verdict line":                           {"## Actions\n\n1. Fix it.\n", "no Verdict: line"},
	} {
		got := reviewActionsProblem(tc.text)
		if tc.wantIn == "" {
			require.Empty(t, got, name)
		} else {
			require.Contains(t, got, tc.wantIn, name)
		}
	}
}

func outcome751(t *testing.T, body string, opted bool) *StepOutcome {
	t.Helper()
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "artifacts", "out"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "artifacts", "out", "review.md"), []byte(body), 0o600))
	return &StepOutcome{StepID: "review", WorkspaceDir: ws, StepStart: time.Now().Add(-time.Minute),
		Step: &registry.WorkflowStep{RequireOutputGlob: "artifacts/out/review.md", RequireReviewActions: opted}}
}

func TestReviewActionsParticipant751(t *testing.T) {
	e := &Executor{}
	ctx := context.Background()
	require.False(t, e.reviewActionsParticipant(ctx, outcome751(t, goodReview751, true)).Refused)

	v := e.reviewActionsParticipant(ctx, outcome751(t, "Verdict: NOT GREEN.\n\n## Findings\n\n- a bug\n", true))
	require.True(t, v.Refused)
	require.True(t, strings.HasPrefix(v.Reason, "schema violation:"), "the existing corrective retry keys on this prefix: %q", v.Reason)
	require.Contains(t, v.Reason, "## Actions")
	require.Contains(t, v.Reason, "No action: ship.")

	require.False(t, e.reviewActionsParticipant(ctx, outcome751(t, "evidence only", false)).Refused,
		"a step that did not opt in is not checked")
}

// The shipped workflows opt in, and their prompts state the format the check
// enforces, so the two travel together in one file.
func TestReviewActionsShippedWorkflows751(t *testing.T) {
	for _, wf := range []string{"companion-architectural-review", "companion-doc-review"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "workflows", wf+".md"))
		require.NoError(t, err)
		front, body, ok := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n---")
		require.True(t, ok, wf)
		var doc struct {
			Steps map[string]registry.WorkflowStep `yaml:"steps"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(front), &doc), wf)
		require.True(t, doc.Steps["review"].RequireReviewActions, "%s: review step must set require_review_actions", wf)
		require.Contains(t, body, "## Actions", wf)
		require.Contains(t, body, "No action: ship.", wf)
	}
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "configs", "project-templates", "companion", "swarm.md.tmpl"))
	require.NoError(t, err)
	_, reviewer, ok := strings.Cut(string(tmpl), "### reviewer")
	require.True(t, ok)
	reviewer, _, _ = strings.Cut(reviewer, "### analyst")
	require.Contains(t, reviewer, "## Actions")
	require.NotContains(t, reviewer, "Suggestions", "the actions say it once")
}

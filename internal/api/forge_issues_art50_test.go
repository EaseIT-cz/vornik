package api

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

type closeCapturingProvider struct {
	issueTestProvider
	spec forge.CloseIssueSpec
}

func (p *closeCapturingProvider) CloseIssue(_ context.Context, _ string, _ int, s forge.CloseIssueSpec) error {
	p.spec = s
	return nil
}

// audit 2026-10-12: GitHub #342 row 4 — the duplicate-close comment must carry
// the OPERATOR'S configured publication notice (ai_disclosure.publication_text),
// so the daemon hands it to the provider rather than the provider guessing.
func TestArt50_ApplyForgeIssueHandsTheConfiguredNoticeToClose(t *testing.T) {
	p := &closeCapturingProvider{}
	notice := aidisclosure.Notice{Text: "OPERATOR NOTICE"}
	if _, err := applyForgeIssue(context.Background(), p, p, registry.ProjectForgeIssues{}, forgeIssueRequest{
		Operation: "close_issue", Repo: "o/r", Number: 1, DuplicateOf: 2, Explanation: "same"}, notice); err != nil {
		t.Fatal(err)
	}
	if p.spec.Notice != notice {
		t.Fatalf("CloseIssue got notice %+v, want the configured %+v", p.spec.Notice, notice)
	}
}

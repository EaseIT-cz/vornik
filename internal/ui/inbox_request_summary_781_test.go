package ui

// GitHub #781 (outcome-inbox-design.md, amendment 2026-11-15): a "My
// requests" card's only text was the run's narration line, which reads as
// its first steps; the card now leads with a snippet of what was asked,
// through the landing page's extractor, and keeps the status as a muted
// second line. Found at: production (operator, 2026-11-15). Regression test
// at: integration (the card builder over its repositories, and the real
// template).

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func summaryCard781(t *testing.T, payload string) (requestCard, string) {
	t.Helper()
	now := time.Now()
	task := &persistence.Task{ID: "req-781", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now, UpdatedAt: now, Payload: []byte(payload)}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
		return mocks.FilterTasks(seed, f), nil
	}}
	execRepo := &mocks.MockExecutionRepository{GetByTaskIDsFunc: func(context.Context, []string) (map[string]*persistence.Execution, error) {
		return map[string]*persistence.Execution{"req-781": {ID: "exec-781", TaskID: "req-781"}}, nil
	}}
	narration := &fakeBatchNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{
		"exec-781": {{ExecutionID: "exec-781", Seq: 0, Text: "Searching memory — step 1 of 4"}},
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithExecutionNarrationRepository(narration))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	var b strings.Builder
	if err := srv.templates.ExecuteTemplate(&b, "requestCardItem", cards[0]); err != nil {
		t.Fatal(err)
	}
	return cards[0], b.String()
}

func TestRequestCard781_LeadsWithThePrompt(t *testing.T) {
	card, html := summaryCard781(t, `{"args":{"prompt":"Find three Italian restaurants near Karlín for Friday"}}`)
	if card.Summary != "Find three Italian restaurants near Karlín for Friday" {
		t.Fatalf("Summary = %q, want the request's prompt", card.Summary)
	}
	i, j := strings.Index(html, "Find three Italian"), strings.Index(html, "Searching memory")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the prompt should come before the status line:\n%s", html)
	}
	if !strings.Contains(html, `data-request-status class="text-xs`) {
		t.Errorf("the status line should be the muted second line:\n%s", html)
	}
}

func TestRequestCard781_NoPromptKeepsTheStatusLine(t *testing.T) {
	card, html := summaryCard781(t, `{"workflow":"digest"}`)
	if card.Summary != "" || !strings.Contains(html, `class="text-sm text-gray-200">Searching memory`) {
		t.Errorf("without a prompt the status line stays the first line:\n%s", html)
	}
}

func TestRequestCard781_LongPromptIsCutAsOnTheLandingPage(t *testing.T) {
	long := strings.Repeat("word ", 60)
	card, _ := summaryCard781(t, `{"args":{"prompt":"`+long+`"}}`)
	if card.Summary != taskSummary([]byte(`{"args":{"prompt":"`+long+`"}}`)) || len(card.Summary) > 145 {
		t.Errorf("Summary = %q, want the landing page's cut", card.Summary)
	}
}

// Design review a2e8 F4: the card's text is taskSummary's, whatever path the
// payload uses, and markup in a prompt renders inert.
func TestRequestCard781_SameExtractorAndInert(t *testing.T) {
	payload := `{"context":{"brief":"Weekly digest of <b>new</b> filings <script>x()</script>"}}`
	card, html := summaryCard781(t, payload)
	if card.Summary == "" || card.Summary != taskSummary([]byte(payload)) {
		t.Fatalf("Summary = %q, want taskSummary's %q", card.Summary, taskSummary([]byte(payload)))
	}
	if strings.Contains(html, "<script>x()") || strings.Contains(html, "<b>new</b>") {
		t.Errorf("markup in a prompt rendered live:\n%s", html)
	}
}

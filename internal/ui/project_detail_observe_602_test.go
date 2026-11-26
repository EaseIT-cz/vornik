package ui

// GitHub #602 (design 11, amendment 2026-11-20, B5): in observe mode the
// project page's autonomy block says so, counts the would-starts of the
// last 7 days, and shows the latest proposals, so the operator can judge
// what autonomy would do before switching it to act.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestPopulateProjectHomepageAutonomy_ObserveSummary_602(t *testing.T) {
	now := time.Now().UTC()
	rows := []*persistence.AutonomyEvaluation{}
	for i := range 7 {
		rows = append(rows, &persistence.AutonomyEvaluation{
			ID: "w", ProjectID: "alpha", Outcome: persistence.AutonomyOutcomeWouldStart,
			ProposedPrompt: "Proposal " + string(rune('A'+i)) + " " + strings.Repeat("x", 300),
			CreatedAt:      now.Add(-time.Duration(i) * time.Hour),
		})
	}
	// Older than 7 days: not counted.
	rows = append(rows, &persistence.AutonomyEvaluation{ID: "old", ProjectID: "alpha", Outcome: persistence.AutonomyOutcomeWouldStart, ProposedPrompt: "stale", CreatedAt: now.Add(-8 * 24 * time.Hour)})
	stub := &stubAutonomyEvalRepo{
		ListFunc: func(_ context.Context, f persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
			if f.Outcome != nil && *f.Outcome == persistence.AutonomyOutcomeWouldStart {
				return rows, nil
			}
			return rows[:1], nil
		},
	}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	project := &registry.Project{ID: "alpha", Autonomy: registry.ProjectAutonomy{Enabled: true, Observe: true}}
	var data ProjectDetailData
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &data)

	if data.ObserveWouldStart7d != 7 {
		t.Fatalf("ObserveWouldStart7d = %d, want 7 (the 8-day-old row is outside the window)", data.ObserveWouldStart7d)
	}
	if len(data.ObserveRecent) != 5 {
		t.Fatalf("ObserveRecent has %d rows, want the latest 5", len(data.ObserveRecent))
	}
	if !strings.HasPrefix(data.ObserveRecent[0].Prompt, "Proposal A") || len(data.ObserveRecent[0].Prompt) > 163 {
		t.Fatalf("latest proposal = %q, want the newest, cut to 160 characters", data.ObserveRecent[0].Prompt)
	}
	if data.ObserveRecent[0].AgoLabel == "" {
		t.Error("each proposal carries a pre-rendered age")
	}

	// Act mode reads nothing extra.
	var act ProjectDetailData
	project.Autonomy.Observe = false
	srv.populateProjectHomepageAutonomy(context.Background(), project, "alpha", &act)
	if act.ObserveWouldStart7d != 0 || act.ObserveRecent != nil {
		t.Fatalf("act mode must not fill the observe summary: %+v", act.ObserveRecent)
	}
}

// Review f6a3: the page cuts by runes, and a count at the scan limit is
// shown as a lower bound ("200+"), not as an exact number.
func TestPopulateObserveSummary_RunesAndCappedCount_602(t *testing.T) {
	now := time.Now().UTC()
	var rows []*persistence.AutonomyEvaluation
	for range observeSummaryScan {
		rows = append(rows, &persistence.AutonomyEvaluation{Outcome: persistence.AutonomyOutcomeWouldStart, ProposedPrompt: strings.Repeat("ž", 200), CreatedAt: now})
	}
	stub := &stubAutonomyEvalRepo{ListFunc: func(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
		return rows, nil
	}}
	srv := NewServer(WithAutonomyEvaluationRepository(stub))
	var data ProjectDetailData
	srv.populateObserveSummary(context.Background(), "alpha", now, &data)
	if !data.ObserveWouldStart7dCapped {
		t.Fatal("a full scan inside the window must mark the count as a lower bound")
	}
	p := data.ObserveRecent[0].Prompt
	if !utf8.ValidString(p) || utf8.RuneCountInString(p) != 160 {
		t.Fatalf("proposal cut to %d runes (valid %v), want 160 valid runes", utf8.RuneCountInString(p), utf8.ValidString(p))
	}
}

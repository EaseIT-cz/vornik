package executor

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestApprovalStep_PausesAsAwaitingApprovalAndSaysSo — audit 2026-10-12,
// GitHub #142 §S7 #13: an approval step recorded its wait as an operator
// pause and emitted no live event, so nothing could tell an approver's wait
// from an operator's stop or say what would release it.
func TestApprovalStep_PausesAsAwaitingApprovalAndSaysSo(t *testing.T) {
	e, _, er, _, tr := setup()
	stub := &stubLivePub{}
	e.livePub = stub
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	var state executionState
	_, paused, err := e.runApprovalStep(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"}, exec,
		"deploy", registry.WorkflowStep{OnSuccess: "done"}, nil, &state)
	if err != nil || !paused {
		t.Fatalf("runApprovalStep: paused=%v err=%v", paused, err)
	}
	if state.PausedReason != PauseReasonAwaitingApproval {
		t.Errorf("PausedReason = %q, want %q", state.PausedReason, PauseReasonAwaitingApproval)
	}
	got := stub.byKind(livepubsub.KindPaused)
	if len(got) != 1 {
		t.Fatalf("live paused events = %d, want 1", len(got))
	}
	if pp, _ := got[0].Payload.(livepubsub.PausedPayload); pp.PauseKind != PauseReasonAwaitingApproval {
		t.Errorf("pause_kind = %q, want %q", pp.PauseKind, PauseReasonAwaitingApproval)
	}

	// Recover must not auto-resume it: only an approver releases it.
	if err := e.ResumePaused("e1"); err == nil {
		t.Error("an approval wait was auto-resumed")
	}
}

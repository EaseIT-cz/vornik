package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestEvaluate_ActiveTasksReasonNamesAnApprovalWait — audit 2026-10-12,
// GitHub #142 §S7 #15: a tick blocked by a task awaiting approval recorded
// "tasks still queued or running", so a project waiting on a human read as a
// busy one, with nothing saying who could unblock it.
func TestEvaluate_ActiveTasksReasonNamesAnApprovalWait(t *testing.T) {
	mk := func(id string, st persistence.TaskStatus) *persistence.Task {
		return &persistence.Task{ID: id, ProjectID: "p1", Status: st, CreatedAt: time.Now(),
			Payload: []byte(`{"taskType":"feature","context":{"prompt":"x"}}`)}
	}
	repo := &mockTaskRepo{tasks: []*persistence.Task{
		mk("a", persistence.TaskStatusAwaitingApproval),
		mk("r", persistence.TaskStatusRunning),
	}}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Enabled: true, Goal: "g", MaxTasksPerHour: 10}}
	require.NoError(t, m.evaluate(context.Background(), p))

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeActiveTasks, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "1 running")
	assert.Contains(t, entries[0].Reason, "1 awaiting approval")
	assert.Contains(t, entries[0].Reason, "/ui/inbox", "the reason must say how to release the approval")
}

// TestAutonomyCircuitOpen_ReasonSeparatesFailedFromCancelled — §S7 #15: the
// trip reason said "%d failed/cancelled", so it could not say whether the
// tasks failed or an operator cancelled them. The trip rule is unchanged
// (open question Q15b).
func TestAutonomyCircuitOpen_ReasonSeparatesFailedFromCancelled(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 5; i++ {
		tasks = append(tasks, mkTerminal(fmt.Sprintf("f%d", i), persistence.TaskStatusFailed, time.Duration(i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		tasks = append(tasks, mkTerminal(fmt.Sprintf("x%d", i), persistence.TaskStatusCancelled, time.Duration(i)*time.Minute))
	}
	tasks = append(tasks, mkTerminal("c0", persistence.TaskStatusCompleted, 30*time.Minute))
	reason, open := autonomyCircuitOpen(tasks)
	require.True(t, open, "7 failed-or-cancelled of 8 still trips: the rule is unchanged")
	assert.Contains(t, reason, "5 failed, 2 cancelled, 1 completed")
}

// TestCreateAutonomousTask_FenceIsRecordedAsLeaderFenced — §S7 #15: a write
// the leader epoch fence refused was recorded NO_ACTION, the outcome for "the
// lead decided no work was needed".
func TestCreateAutonomousTask_FenceIsRecordedAsLeaderFenced(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil,
		WithLeaderGate(fenceGate{verifyOK: false}), WithEvaluationRepository(evalRepo))
	require.NoError(t, m.createTaskErr(context.Background(), fenceTestProject(),
		`{"prompt":"Implement feature X","type":"feature"}`, time.Now()))
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeLeaderFenced, entries[0].Outcome)
}

package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunExecutionCompletedSinceSuite pins ExecutionFilter.CompletedSince on both
// drivers (ui-design-refresh-design.md §14.2, #756): the home page's "Done
// today" counts executions by completed_at, not created_at, because a job
// created yesterday and finished this morning is done today. Tasks carry no
// completion time, and updated_at moves when a sweep touches a row.
func RunExecutionCompletedSinceSuite(t *testing.T, repo persistence.ExecutionRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("p756")
	midnight := time.Now().UTC().Truncate(24 * time.Hour)
	at := func(d time.Duration) *time.Time { v := midnight.Add(d); return &v }
	rows := []struct {
		id        string
		created   time.Time
		completed *time.Time
		status    persistence.ExecutionStatus
	}{
		{"created-yesterday-done-today", midnight.Add(-3 * time.Hour), at(4 * time.Hour), persistence.ExecutionStatusCompleted},
		{"created-and-done-today", midnight.Add(time.Hour), at(3 * time.Hour), persistence.ExecutionStatusCompleted},
		{"done-yesterday", midnight.Add(-5 * time.Hour), at(-time.Hour), persistence.ExecutionStatusCompleted},
		{"running-created-today", midnight.Add(time.Hour), nil, persistence.ExecutionStatusRunning},
	}
	for _, r := range rows {
		// Postgres enforces the execution → task foreign key.
		if err := tasks.Create(ctx, &persistence.Task{ID: project + "-t-" + r.id, ProjectID: project, Status: persistence.TaskStatusCompleted, Payload: []byte(`{}`), CreatedAt: r.created, UpdatedAt: r.created}); err != nil {
			t.Fatalf("create task %s: %v", r.id, err)
		}
		e := &persistence.Execution{
			ID: project + "-" + r.id, TaskID: project + "-t-" + r.id, ProjectID: project,
			Status: r.status, CreatedAt: r.created, CompletedAt: r.completed,
		}
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("create %s: %v", r.id, err)
		}
	}
	since := midnight
	status := persistence.ExecutionStatusCompleted
	got, err := repo.List(ctx, persistence.ExecutionFilter{ProjectID: &project, Status: &status, CompletedSince: &since, PageSize: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	for _, want := range []string{"created-yesterday-done-today", "created-and-done-today"} {
		if !ids[project+"-"+want] {
			t.Errorf("CompletedSince midnight should include %s; got %v", want, ids)
		}
	}
	for _, not := range []string{"done-yesterday", "running-created-today"} {
		if ids[project+"-"+not] {
			t.Errorf("CompletedSince midnight should exclude %s", not)
		}
	}
	if len(got) != 2 {
		t.Errorf("got %d executions, want 2", len(got))
	}
	// Count applies the same bound (the home page counts, it does not list).
	n, err := repo.Count(ctx, persistence.ExecutionFilter{ProjectID: &project, Status: &status, CompletedSince: &since})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
	// NewestCompletedFirst orders by completion: the job created yesterday
	// finished last (04:00), so it leads, although it was created first.
	got, err = repo.List(ctx, persistence.ExecutionFilter{ProjectID: &project, Status: &status, NewestCompletedFirst: true, PageSize: 2})
	if err != nil {
		t.Fatalf("List newest completed: %v", err)
	}
	if len(got) != 2 || got[0].ID != project+"-created-yesterday-done-today" || got[1].ID != project+"-created-and-done-today" {
		var ids []string
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		t.Errorf("NewestCompletedFirst order = %v, want the 04:00 completion first, then 03:00", ids)
	}
}

// RunCountCompletedByProjectSuite pins ExecutionRepository.
// CountCompletedByProject on both drivers (ui-design-refresh-design.md §15.2,
// #757): COMPLETED executions per project with completed_at in [since,
// until), counted by completion time, not creation time.
func RunCountCompletedByProjectSuite(t *testing.T, repo persistence.ExecutionRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	pa, pb := uniqueID("p757a"), uniqueID("p757b")
	day := time.Now().UTC().Truncate(24 * time.Hour)
	since, until := day.Add(-48*time.Hour), day.Add(time.Hour)
	at := func(d time.Duration) *time.Time { v := day.Add(d); return &v }
	rows := []struct {
		id, project string
		created     time.Time
		completed   *time.Time
		status      persistence.ExecutionStatus
	}{
		{"a-old-created-done-in-window", pa, day.Add(-10 * 24 * time.Hour), at(-24 * time.Hour), persistence.ExecutionStatusCompleted},
		{"a-done-in-window", pa, day.Add(-30 * time.Hour), at(-2 * time.Hour), persistence.ExecutionStatusCompleted},
		{"a-done-before-window", pa, day.Add(-5 * 24 * time.Hour), at(-72 * time.Hour), persistence.ExecutionStatusCompleted},
		{"a-done-after-until", pa, day, at(2 * time.Hour), persistence.ExecutionStatusCompleted},
		{"a-failed-in-window", pa, day.Add(-3 * time.Hour), at(-time.Hour), persistence.ExecutionStatusFailed},
		{"b-done-in-window", pb, day.Add(-5 * time.Hour), at(-4 * time.Hour), persistence.ExecutionStatusCompleted},
		// The half-open window's edges: exactly since counts, exactly until does not.
		{"b-done-at-since", pb, day.Add(-50 * time.Hour), at(-48 * time.Hour), persistence.ExecutionStatusCompleted},
		{"b-done-at-until", pb, day, at(time.Hour), persistence.ExecutionStatusCompleted},
	}
	for _, r := range rows {
		if err := tasks.Create(ctx, &persistence.Task{ID: r.project + "-t-" + r.id, ProjectID: r.project, Status: persistence.TaskStatusCompleted, Payload: []byte(`{}`), CreatedAt: r.created, UpdatedAt: r.created}); err != nil {
			t.Fatalf("create task %s: %v", r.id, err)
		}
		if err := repo.Create(ctx, &persistence.Execution{ID: r.project + "-" + r.id, TaskID: r.project + "-t-" + r.id, ProjectID: r.project, Status: r.status, CreatedAt: r.created, CompletedAt: r.completed}); err != nil {
			t.Fatalf("create %s: %v", r.id, err)
		}
	}
	got, err := repo.CountCompletedByProject(ctx, since, until)
	if err != nil {
		t.Fatalf("CountCompletedByProject: %v", err)
	}
	if got[pa] != 2 || got[pb] != 2 {
		t.Errorf("counts = %v, want %s:2 (completion in window, creation before it counts) and %s:2 (in window, and exactly at since; not exactly at until)", got, pa, pb)
	}
}

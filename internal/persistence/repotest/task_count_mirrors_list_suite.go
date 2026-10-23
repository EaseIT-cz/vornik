package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunTaskCountMirrorsListSuite pins TaskRepository.Count to TaskRepository.List:
// for every TaskFilter field, Count(f) is the number of rows an unpaged
// List(f) returns. GitHub #411 (web-ui-design.md "Task list (/ui/tasks) states
// its cut — 2026-10-17", part A): Count claimed to mirror List's WHERE clauses
// but applied only ProjectID and Status/Statuses on both drivers, so a page
// counting with a recency window or a project set would publish a wrong
// denominator — measured on SQLite, UpdatedSince(24h) listed 10 rows and
// counted 130. The /ui/tasks "Showing X–Y of M" line takes M from Count.
func RunTaskCountMirrorsListSuite(t *testing.T, repo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	pa, pb, pc := uniqueID("cnt411-a"), uniqueID("cnt411-b"), uniqueID("cnt411-c")
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)

	mk := func(project string, st persistence.TaskStatus, at time.Time) *persistence.Task {
		t.Helper()
		task := newQueuedTask(project)
		task.Status = st
		task.CreatedAt, task.UpdatedAt = at, at
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create: %v", err)
		}
		return task
	}
	var ids []string
	for i := 0; i < 4; i++ { // old, untouched: outside every recent window
		ids = append(ids, mk(pa, persistence.TaskStatusQueued, old.Add(time.Duration(i)*time.Minute)).ID)
	}
	recent := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < 3; i++ {
		ids = append(ids, mk(pa, persistence.TaskStatusCompleted, recent.Add(time.Duration(i)*time.Second)).ID)
	}
	for i := 0; i < 2; i++ {
		ids = append(ids, mk(pb, persistence.TaskStatusWaitingForChildren, old).ID)
	}
	failed := mk(pb, persistence.TaskStatusQueued, old)
	// UpdateStatus(FAILED) stamps failed_at and updated_at = now.
	if err := repo.UpdateStatus(ctx, failed.ID, persistence.TaskStatusFailed); err != nil {
		t.Fatalf("fail: %v", err)
	}
	ids = append(ids, failed.ID)
	mk(pc, persistence.TaskStatusQueued, recent) // a third project, never selected by ProjectIDs below

	since := time.Now().UTC().Add(-time.Hour)
	before := time.Now().UTC().Add(-24 * time.Hour)
	pAP := pa
	queued := persistence.TaskStatusQueued
	both := []string{pa, pb}

	cases := []struct {
		name string
		f    persistence.TaskFilter
		want int
	}{
		{"ProjectIDs", persistence.TaskFilter{ProjectIDs: both}, 10},
		{"ProjectIDs_none_match", persistence.TaskFilter{ProjectIDs: []string{uniqueID("cnt411-none")}}, 0},
		{"IDs", persistence.TaskFilter{IDs: ids[:3]}, 3},
		{"UpdatedSince", persistence.TaskFilter{ProjectIDs: both, UpdatedSince: &since}, 4},
		{"UpdatedBefore", persistence.TaskFilter{ProjectIDs: both, UpdatedBefore: &before}, 6},
		{"FailedSince", persistence.TaskFilter{ProjectIDs: both, FailedSince: &since}, 1},
		{"ProjectID_and_Status", persistence.TaskFilter{ProjectID: &pAP, Status: &queued}, 4},
		{"Statuses_active_set", persistence.TaskFilter{ProjectIDs: both, Statuses: persistence.CancellableTaskStatuses()}, 6},
		{"Statuses_empty_nonnil_is_no_constraint", persistence.TaskFilter{ProjectIDs: both, Statuses: []persistence.TaskStatus{}}, 10},
		{"Statuses_wins_over_Status", persistence.TaskFilter{ProjectIDs: both, Status: &queued, Statuses: []persistence.TaskStatus{persistence.TaskStatusCompleted}}, 3},
		{"ProjectID_and_ProjectIDs_both_apply", persistence.TaskFilter{ProjectID: &pAP, ProjectIDs: []string{pb}}, 0},
		{"Window_and_Statuses", persistence.TaskFilter{ProjectIDs: both, UpdatedSince: &since, Statuses: []persistence.TaskStatus{persistence.TaskStatusCompleted}}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := repo.List(ctx, tc.f)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			n, err := repo.Count(ctx, tc.f)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if len(rows) != tc.want {
				t.Fatalf("List returned %d rows, fixture expects %d", len(rows), tc.want)
			}
			if n != int64(len(rows)) {
				t.Fatalf("Count = %d, List = %d rows: Count does not mirror List (#411)", n, len(rows))
			}
		})
	}

	// Paging never changes Count: it is the denominator of the paged list.
	paged := persistence.TaskFilter{ProjectIDs: both, PageSize: 3, Offset: 3}
	if n, err := repo.Count(ctx, paged); err != nil || n != 10 {
		t.Fatalf("Count with PageSize/Offset = (%d, %v), want 10", n, err)
	}
}

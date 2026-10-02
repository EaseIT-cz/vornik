package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/brokerschedule"
	"vornik.io/vornik/internal/cronexpr"
	"vornik.io/vornik/internal/registry"
)

// recentRunCount is how many past slots list_my_setup shows (design §17.4).
const recentRunCount = 5

// scheduleView is list_my_setup's view of one schedule.
func (s *agentAdminService) scheduleView(ctx context.Context, projectID, workflowID string, sc *registry.BrokerSchedule, approved bool, now time.Time) *SetupSchedule {
	tz := strings.TrimSpace(sc.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	inputs, _ := json.Marshal(sc.Inputs)
	v := &SetupSchedule{Cron: sc.Cron, Timezone: tz, Inputs: inputs, Sentence: agentadmin.DescribeSchedule(sc), Approved: approved, RecentRuns: []SetupRun{}}
	loc, err := sc.Location()
	if err != nil {
		return v
	}
	if next, err := cronexpr.NextFireAtIn(sc.Cron, now, loc); err == nil {
		v.NextRun = &next
	}
	since := now.AddDate(-1, 0, 0)
	if row, err := s.grants.GetWorkflowReach(ctx, workflowID); err == nil && row.ApprovedAt.After(since) {
		since = row.ApprovedAt
	}
	v.RecentRuns = s.recentRuns(ctx, projectID, workflowID, sc, since, now)
	return v
}

// recentRuns lists the last slots between since and now, newest first, each
// looked up by the key the scheduler uses, so a slot that never ran shows as
// not_run. Only this workflow's own tasks can appear: the key names it.
func (s *agentAdminService) recentRuns(ctx context.Context, projectID, workflowID string, sc *registry.BrokerSchedule, since, now time.Time) []SetupRun {
	loc, err := sc.Location()
	if err != nil {
		return []SetupRun{}
	}
	compiled, err := cronexpr.Compile(sc.Cron, loc)
	if err != nil {
		return []SetupRun{}
	}
	var slots []time.Time
	seen := map[string]bool{}
	for slot := compiled.Next(since); !slot.IsZero() && !slot.After(now); slot = compiled.Next(slot) {
		key := brokerschedule.SlotKey(workflowID, slot, loc)
		if seen[key] { // a fall-back day's repeated hour is one slot
			continue
		}
		seen[key] = true
		slots = append(slots, slot)
		if len(slots) > recentRunCount {
			slots = slots[1:]
		}
	}
	out := make([]SetupRun, 0, len(slots))
	for i := len(slots) - 1; i >= 0; i-- {
		run := SetupRun{Slot: slots[i], Status: "not_run"}
		if s.c.repos != nil && s.c.repos.Tasks != nil {
			if task, err := s.c.repos.Tasks.GetByIdempotencyKey(ctx, projectID, brokerschedule.SlotKey(workflowID, slots[i], loc)); err == nil && task != nil {
				run.TaskID, run.Status = task.ID, strings.ToLower(string(task.Status))
			}
		}
		out = append(out, run)
	}
	return out
}

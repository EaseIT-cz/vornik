package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/brokerschedule"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered Vornik design §17, plan P7.4, end to end in the
// service: the agent defines a workflow with a schedule, the device
// approves it, the scheduler's source reports it approved, a fired slot
// creates exactly one SCHEDULED task with the approved inputs, and
// list_my_setup shows the schedule and the run. Control: scheduleSource,
// FireScheduledBroker and the setup's schedule view together.
func TestAgentSchedule_EndToEnd(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	if res := f.do(agentadmin.VerbDefineSwarm, agentadmin.DefineSwarmInput{Slug: "finance", Roles: []agentadmin.RoleInput{
		{Name: "reader", Instructions: "Read.", Tools: []string{"file_write"}}}}); res.Effect != agentadmin.EffectApplied {
		t.Fatalf("define_swarm: %+v", res)
	}
	sched := &agentadmin.ScheduleInput{Cron: "0 8 1 * *", Timezone: "Europe/Prague", Inputs: json.RawMessage(`{"month":"previous"}`)}
	res := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "finance", Slug: "summary", Purpose: "monthly summary",
		Steps:    []agentadmin.StepInput{{Name: "sum", Role: "reader", Instructions: "Summarise the month."}},
		Inputs:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"month":{"enum":["current","previous"]}}}`),
		Egress:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`),
		Schedule: sched})
	src := scheduleSource{c: f.c}
	entries, err := src.Scheduled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a schedule awaiting approval is already live: %+v", entries)
	}
	f.approve(res)
	f.svc.bg.Wait()

	entries, _ = src.Scheduled(ctx)
	if len(entries) != 1 || !entries[0].Approved || entries[0].Cron != "0 8 1 * *" || entries[0].ApprovedAt.IsZero() {
		t.Fatalf("source after approval: %+v", entries)
	}
	wid := entries[0].WorkflowID

	// A slot fires through the API server, twice (two ticks): one task.
	loc, _ := time.LoadLocation("Europe/Prague")
	slot := time.Date(2026, 11, 1, 8, 0, 0, 0, loc)
	key := brokerschedule.SlotKey(wid, slot, loc)
	first, err := scheduleFirer{c: f.c}.FireScheduledBroker(ctx, wid, key)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	second, err := scheduleFirer{c: f.c}.FireScheduledBroker(ctx, wid, key)
	if err != nil || second != first {
		t.Fatalf("second fire: %s %v, want %s", second, err, first)
	}
	task, err := f.c.repos.Tasks.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if task.CreationSource != persistence.TaskCreationSourceScheduled || !strings.Contains(string(task.Payload), `"month":"previous"`) {
		t.Fatalf("task: %s %s", task.CreationSource, task.Payload)
	}

	// list_my_setup shows the schedule, its next run, and the run.
	v := setupOf(t, f)
	p := projectView(v, "hermes--finance")
	var wf *SetupWorkflow
	for i := range p.Workflows {
		if p.Workflows[i].ID == wid {
			wf = &p.Workflows[i]
		}
	}
	if wf == nil || wf.Schedule == nil {
		t.Fatalf("no schedule in list_my_setup: %+v", p.Workflows)
	}
	s := wf.Schedule
	if s.Cron != "0 8 1 * *" || !strings.Contains(s.Sentence, "on day 1 of every month") || s.NextRun == nil || !s.Approved {
		t.Fatalf("schedule view: %+v", s)
	}
	if len(s.RecentRuns) > 5 {
		t.Fatalf("recent runs: %d", len(s.RecentRuns))
	}
	// As of just after the fired slot, the run list has it, by its slot's
	// key; the slot before it (never fired) shows as not run (§17.5).
	runs := f.svc.recentRuns(ctx, "hermes--finance", wid, f.c.Registry.GetWorkflow(wid).Broker.Schedule, entries[0].ApprovedAt.AddDate(0, -3, 0), slot.Add(time.Minute))
	if len(runs) == 0 || runs[0].TaskID != first || runs[0].Status == "not_run" {
		t.Fatalf("runs as of the slot: %+v", runs)
	}
	if len(runs) > 1 && runs[1].Status != "not_run" {
		t.Fatalf("an unfired earlier slot: %+v", runs[1])
	}
}

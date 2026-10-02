package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

func scheduledWF(tr *tree, sched *ScheduleInput) Change {
	return tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "finance", Slug: "spend", Purpose: "spending",
		Steps:    []StepInput{{Name: "read", Role: "reader", Instructions: "Summarise the month's spending."}},
		Inputs:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"month":{"type":"string","enum":["current","previous"]}}}`),
		Egress:   egress1(),
		Schedule: sched})
}

func scheduleTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{{Name: "reader", Instructions: "Read.", Tools: []string{"file_write"}}}}))
	return tr
}

// Agent-administered Vornik design §17.1-17.2: a schedule renders into
// broker.schedule, is widening, is part of the reach signature (cron, zone
// and inputs), and removing it applies at once. Control: SignatureOf's
// Schedule and the schedule rendering.
func TestDefineWorkflow_Schedule(t *testing.T) {
	tr := scheduleTree(t)
	monthly := &ScheduleInput{Cron: "0 8 1 * *", Timezone: "Europe/Prague", Inputs: json.RawMessage(`{"month":"previous"}`)}
	c := scheduledWF(tr, monthly)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Ops[0].Content, "schedule:") || !strings.Contains(c.Ops[0].Content, `"0 8 1 * *"`) {
		t.Fatalf("rendered:\n%s", c.Ops[0].Content)
	}
	for _, want := range []string{"automatically", "at 08:00 on day 1 of every month", "Europe/Prague", `"month":"previous"`} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("the approval sentence does not say %q: %s", want, c.Sentence)
		}
	}
	tr.apply(c)
	tr.mustClass(scheduledWF(tr, monthly), Inert)

	// Each part of the schedule is in the signature.
	for name, s := range map[string]*ScheduleInput{
		"cron":   {Cron: "0 9 1 * *", Timezone: "Europe/Prague", Inputs: json.RawMessage(`{"month":"previous"}`)},
		"zone":   {Cron: "0 8 1 * *", Timezone: "Europe/London", Inputs: json.RawMessage(`{"month":"previous"}`)},
		"inputs": {Cron: "0 8 1 * *", Timezone: "Europe/Prague", Inputs: json.RawMessage(`{"month":"current"}`)},
	} {
		if got := scheduledWF(tr, s); got.Class != Widening {
			t.Errorf("changing the schedule's %s is %v, want widening", name, got.Class)
		}
	}
	// Removing the schedule changes the signature, and only an approval
	// writes a signature: inert changes never write approvals (design §17.2,
	// amended during the build). The sentence says it stops automatic runs;
	// remove on the workflow stops it at once.
	stop := scheduledWF(tr, nil)
	tr.mustClass(stop, Widening)
	if !strings.Contains(stop.Sentence, "no longer run automatically") {
		t.Errorf("the stop sentence: %s", stop.Sentence)
	}
}

// An omitted zone and "UTC" are the same schedule, and the inputs' key order
// does not matter (review 6a3f, minor).
func TestDefineWorkflow_ScheduleCanonical(t *testing.T) {
	tr := scheduleTree(t)
	tr.apply(scheduledWF(tr, &ScheduleInput{Cron: "0 8 * * *", Inputs: json.RawMessage(`{"month":"previous"}`)}))
	tr.mustClass(scheduledWF(tr, &ScheduleInput{Cron: "0 8 * * *", Timezone: "UTC", Inputs: json.RawMessage(`{ "month" : "previous" }`)}), Inert)
}

// Refusals name the rule, never an input value.
func TestDefineWorkflow_ScheduleRefusals(t *testing.T) {
	tr := scheduleTree(t)
	for name, c := range map[string]struct {
		s    *ScheduleInput
		says string
	}{
		"sub-hourly":    {&ScheduleInput{Cron: "*/15 * * * *", Inputs: json.RawMessage(`{"month":"previous"}`)}, "at most hourly"},
		"descriptor":    {&ScheduleInput{Cron: "@daily", Inputs: json.RawMessage(`{"month":"previous"}`)}, "cron"},
		"bad zone":      {&ScheduleInput{Cron: "0 8 * * *", Timezone: "Nowhere/Land", Inputs: json.RawMessage(`{"month":"previous"}`)}, "timezone"},
		"bad input":     {&ScheduleInput{Cron: "0 8 * * *", Inputs: json.RawMessage(`{"month":"SECRETVALUE"}`)}, "month"},
		"inputs object": {&ScheduleInput{Cron: "0 8 * * *", Inputs: json.RawMessage(`["x"]`)}, "inputs"},
		"inputs huge":   {&ScheduleInput{Cron: "0 8 * * *", Inputs: json.RawMessage(`{"month":"` + strings.Repeat("x", 5000) + `"}`)}, "bytes"},
	} {
		got := scheduledWF(tr, c.s)
		if got.Class != Refused || !strings.Contains(got.Reason, c.says) || strings.Contains(got.Reason, "SECRETVALUE") {
			t.Errorf("%s: %v %q", name, got.Class, got.Reason)
		}
	}
}

// Review 6a3f R6: the approval sentence is what the person approves, so the
// describer's matrix is pinned: plain words for single values, lists and *;
// the expression itself for anything else. Control: describeCron.
func TestDescribeCron(t *testing.T) {
	for expr, want := range map[string]string{
		"0 8 * * *":    "at 08:00 every day",
		"0 8 1 * *":    "at 08:00 on day 1 of every month",
		"30 7 * * 1,3": "at 07:30 on Monday and Wednesday",
		"0 8 1,15 * *": "at 08:00 on days 1 and 15 of every month",
		"0 6 1 1 *":    "at 06:00 on day 1 of January",
		"0 18 * * 0":   "at 18:00 on Sunday",
		"0 9 * 3,6 1":  "at 09:00 on Monday in March and June",
		"0 * * * *":    "at minute 0 of every hour",
		"0 */3 * * *":  "on the cron schedule `0 */3 * * *`",
		"0 8 * * 1-5":  "on the cron schedule `0 8 * * 1-5`",
		"0 8 * * MON":  "on the cron schedule `0 8 * * MON`",
		"0 8 1 * 1":    "on the cron schedule `0 8 1 * 1`", // day-of-month OR day-of-week: not described
		"0,30 8 * * *": "on the cron schedule `0,30 8 * * *`",
	} {
		if got := describeCron(expr); got != want {
			t.Errorf("%s: %q, want %q", expr, got, want)
		}
	}
}

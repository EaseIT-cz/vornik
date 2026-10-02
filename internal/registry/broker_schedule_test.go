package registry

import (
	"strings"
	"testing"
)

func brokerWithSchedule(t *testing.T, schedule string) *WorkflowBroker {
	t.Helper()
	return parseBroker(t, validBrokerYAML+"schedule:\n"+schedule)
}

// Agent-administered Vornik design §17.1: a schedule is a 5-field cron, an
// IANA zone and fixed inputs that satisfy the workflow's input schema, at
// most hourly. The loader refuses what the renderer would refuse, so a hand
// edit cannot slip one in. Control: WorkflowBroker.Validate's schedule check.
func TestBrokerValidate_Schedule(t *testing.T) {
	ok := brokerWithSchedule(t, "  cron: \"0 8 1 * *\"\n  timezone: Europe/Prague\n  inputs: { since: \"2026-01-01T00:00:00Z\", limit: 5 }\n")
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid schedule was refused: %v", err)
	}
	if ok.Schedule == nil || ok.Schedule.Cron != "0 8 1 * *" || ok.Schedule.Inputs["limit"] == nil {
		t.Fatalf("parsed: %+v", ok.Schedule)
	}
	for name, c := range map[string]struct{ yaml, says string }{
		"sub-hourly":    {"  cron: \"*/30 * * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\" }\n", "at most hourly"},
		"descriptor":    {"  cron: \"@daily\"\n  inputs: { since: \"2026-01-01T00:00:00Z\" }\n", "cron"},
		"six fields":    {"  cron: \"0 0 8 * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\" }\n", "cron"},
		"bad zone":      {"  cron: \"0 8 * * *\"\n  timezone: Mars/Olympus\n  inputs: { since: \"2026-01-01T00:00:00Z\" }\n", "timezone"},
		"missing input": {"  cron: \"0 8 * * *\"\n  inputs: { limit: 5 }\n", "since"},
		"input too big": {"  cron: \"0 8 * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\", limit: 99 }\n", "limit"},
		"unknown input": {"  cron: \"0 8 * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\", other: 1 }\n", "inputs"},
		"no cron":       {"  inputs: { since: \"2026-01-01T00:00:00Z\" }\n", "cron"},
	} {
		err := brokerWithSchedule(t, c.yaml).Validate()
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a refusal mentioning %q", name, err, c.says)
		}
	}
}

// A schedule's refusal names the field and the rule, never the value: the
// inputs came from an agent.
func TestBrokerValidate_ScheduleRefusalNamesNoValue(t *testing.T) {
	err := brokerWithSchedule(t, "  cron: \"0 8 * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\", topic: \""+strings.Repeat("SECRETVALUE", 20)+"\" }\n").Validate()
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("refusal: %v", err)
	}
}

// Design §17.1: schedules are for agent workflows in this release; an
// operator broker workflow carrying one is refused at load. Control:
// checkOperatorWorkflowReach's schedule rule.
func TestOperatorWorkflowScheduleRefused(t *testing.T) {
	b := brokerWithSchedule(t, "  cron: \"0 8 * * *\"\n  inputs: { since: \"2026-01-01T00:00:00Z\" }\n")
	if err := checkAgentWorkflow("finance-digest", &Workflow{ID: "finance-digest", Broker: b}); err == nil || !strings.Contains(err.Error(), "agent workflows") {
		t.Fatalf("operator workflow with a schedule: %v", err)
	}
	if err := checkAgentWorkflow("hermes--fin--digest", &Workflow{ID: "hermes--fin--digest", Broker: b}); err != nil {
		t.Fatalf("agent workflow with a schedule: %v", err)
	}
}

package service

import (
	"encoding/json"
	"sort"
	"strings"
	"vornik.io/vornik/internal/agenttools"

	"context"
	"reflect"
	"testing"
	"vornik.io/vornik/internal/agentadmin"

	"vornik.io/vornik/internal/registry"
)

// Agent-administered design §18.2 (2026-10-02): the agent learned the input
// rules and the one-pending-change rule only from refusals. describe_installation
// now states them, from the validator's own source.
func TestAgentAdmin_DescribeStatesTheRulesBeforeAnyRefusal(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caps.InputRules, registry.BrokerInputRules()) {
		t.Fatalf("input_rules = %+v, want the validator's rules", caps.InputRules)
	}
	if !caps.OnePendingChangePerProject {
		t.Fatal("one_pending_change_per_project is not stated")
	}
}

// Design §18.2 (review 3e94 F10): a change refused because another waits
// names that request, so the agent can tell the user which approval it
// waits for.
func TestAgentAdmin_PendingChangeRefusalNamesTheRequest(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	in := agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps:  []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)}
	first := f.do(agentadmin.VerbDefineWorkflow, in)
	if first.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("first: %+v", first)
	}
	id := first.ApprovalURL[strings.LastIndex(first.ApprovalURL, "/")+1:]
	second := f.do(agentadmin.VerbDefineWorkflow, in)
	if second.Effect != agentadmin.EffectRefused || !strings.Contains(second.Reason, id) || !strings.Contains(second.Reason, "widening_change") {
		t.Fatalf("the refusal does not name the waiting request %s: %+v", id, second)
	}
}

// Design §18.7: the approval page's plain summary and level come from the
// request's own approved document, through the describer this service
// registers for widening changes and credential slots.
func TestAgentAdmin_ApprovalPageDescribesTheRequest(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	res := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps:  []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)})
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	d := f.c.approverDeviceService().Describe(*req)
	if d == nil || d.Level != agentadmin.LevelLow || !strings.Contains(d.Summary, "add a skill") || len(d.Reasons) == 0 {
		t.Fatalf("description: %+v", d)
	}
}

// Design §18.8: describe_installation states the tools every role receives
// beyond those it declares, so an agent knows its roles have them.
func TestAgentAdmin_DescribeStatesTheAlwaysGrantedTools(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	want := agenttools.EveryRole()
	sort.Strings(want)
	got := append([]string(nil), caps.AlwaysGranted...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("always_granted = %v, want %v", got, want)
	}
}

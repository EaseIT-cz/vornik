package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #172 item 3 (2026-10-13): an applied agent change must not stand
// when a write that records it fails. Design §23.3 (as built: the engine
// reverts the files when the proposal ledger commit fails; an inert change's
// post-ledger reach delete was left half-applied) and §23.4 (the fix: roll
// the change back).

// failingGrants fails one write of the agent grant repository.
type failingGrants struct {
	persistence.AgentGrantRepository
	deleteReach, upsertReach error
}

func (g *failingGrants) DeleteWorkflowReach(ctx context.Context, wf string) error {
	if g.deleteReach != nil {
		return g.deleteReach
	}
	return g.AgentGrantRepository.DeleteWorkflowReach(ctx, wf)
}

func (g *failingGrants) UpsertWorkflowReach(ctx context.Context, a persistence.AgentWorkflowApproval) error {
	if g.upsertReach != nil {
		return g.upsertReach
	}
	return g.AgentGrantRepository.UpsertWorkflowReach(ctx, a)
}

// failingLedger fails the proposal ledger commit.
type failingLedger struct {
	persistence.ApplyJournalRepository
}

func (failingLedger) MarkAppliedWithLedger(context.Context, persistence.JournalLedgerCommit) error {
	return errors.New("injected: ledger commit failed")
}

var spendEgress = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)

// financeWithWorkflow is a namespace with project finance and an approved
// workflow hermes--finance--spend.
func financeWithWorkflow(t *testing.T) *agentAdminFixture {
	t.Helper()
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps: []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}}, Egress: spendEgress}))
	if f.c.Registry.GetWorkflow("hermes--finance--spend") == nil {
		t.Fatal("fixture: the workflow did not load")
	}
	return f
}

// A1 (the defect): removing a workflow whose reach delete fails used to
// answer an error with the file deleted, the ledger APPLIED and the reach row
// left behind. Now the change is rolled back.
func TestAgentAdminLedger_InertChangeRolledBackWhenItsRecordFails(t *testing.T) {
	f := financeWithWorkflow(t)
	ctx := context.Background()
	f.svc.grants = &failingGrants{AgentGrantRepository: f.svc.grants, deleteReach: errors.New("injected: reach delete failed")}

	raw, _ := json.Marshal(agentadmin.RemoveInput{Kind: "workflow", ID: "finance--spend"})
	res, err := f.svc.Do(ctx, f.key, agentadmin.VerbRemove, raw)
	if err != nil {
		t.Fatalf("an error instead of a refusal (the half-applied answer): %v", err)
	}
	if res.Effect != agentadmin.EffectRefused || res.Rule != agentadmin.RuleRecordFailed || !strings.Contains(res.Reason, "rolled back") {
		t.Fatalf("result %+v, want refused record_failed naming the rollback (#334 audit 2026-10-14: a daemon-side record failure is not the agent's refusal)", res)
	}
	if f.c.Registry.GetWorkflow("hermes--finance--spend") == nil {
		t.Fatal("the workflow is gone although its removal was not recorded")
	}
	p, err := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal %+v, %v; want ROLLED_BACK", p, err)
	}
	if _, err := f.c.repos.AgentGrants.GetWorkflowReach(ctx, "hermes--finance--spend"); err != nil {
		t.Fatalf("the reach row is gone: %v", err)
	}
}

// Engine half, pinned as holding (§23.3): a create_project whose ledger
// commit fails is refused and leaves no file and no APPLIED proposal.
func TestAgentAdminLedger_FailedLedgerCommitLeavesNoChange(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	if f.svc.engine.Journal == nil {
		t.Fatal("fixture: the engine has no journal; this test pins the journaled path")
	}
	f.svc.engine.Journal = failingLedger{ApplyJournalRepository: f.svc.engine.Journal}
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	if res.Effect != agentadmin.EffectRefused {
		t.Fatalf("a change whose ledger commit failed was answered %+v", res)
	}
	if f.c.Registry.GetProject("hermes--finance") != nil {
		t.Fatal("the project loaded although its ledger row failed")
	}
	if _, err := os.Stat(filepath.Join(f.svc.configsDir, "projects", "hermes--finance.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the project file is on disk: %v", err)
	}
	p, err := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if err != nil || p.Status == persistence.ProposalStatusApplied {
		t.Fatalf("proposal %+v, %v; must not be APPLIED", p, err)
	}
}

// A3, pinned as holding (§23.3): a phone approval whose grant write fails is
// owed durably (approved, not applied) and a second run records it.
func TestAgentAdminLedger_ApprovedChangeOwesItsGrantDurably(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	res := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps: []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}}, Egress: spendEgress})
	orig := f.svc.grants
	f.svc.grants = &failingGrants{AgentGrantRepository: orig, upsertReach: errors.New("injected: grant write failed")}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.approverDeviceService().Decide(ctx, f.device, id, req.RenderedSHA256, true); err == nil {
		t.Fatal("the effect's grant write failed but the decision reported success")
	}
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err != nil || row.Status != persistence.ApprovalApproved || row.AppliedAt != nil {
		t.Fatalf("request %+v, %v; want approved and not applied (owed)", row, err)
	}
	if _, err := f.c.repos.AgentGrants.GetWorkflowReach(ctx, "hermes--finance--spend"); err == nil {
		t.Fatal("the grant was recorded although its write failed")
	}
	f.svc.grants = orig
	if err := f.svc.applyApproved(ctx, *row); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if _, err := f.c.repos.AgentGrants.GetWorkflowReach(ctx, "hermes--finance--spend"); err != nil {
		t.Fatalf("the retry did not record the grant: %v", err)
	}
}

// A2: a removal touching a connection runs inside an account transition;
// its reach delete after Finish failing is rolled back the same way, and
// the namespace is left accepting new changes.
func TestAgentAdminLedger_TransitionChangeRolledBackWhenItsRecordFails(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	if f.c.Registry.GetWorkflow("hermes--comms--reply-mail") == nil {
		t.Fatal("fixture: no workflow to remove")
	}
	f.svc.grants = &failingGrants{AgentGrantRepository: f.svc.grants, deleteReach: errors.New("injected: reach delete failed")}
	raw, _ := json.Marshal(agentadmin.RemoveInput{Kind: "project", ID: "comms"})
	res, err := f.svc.Do(ctx, f.key, agentadmin.VerbRemove, raw)
	if err != nil {
		t.Fatalf("an error instead of a refusal (the half-applied answer): %v", err)
	}
	if res.Effect != agentadmin.EffectRefused || res.Rule != agentadmin.RuleRecordFailed || !strings.Contains(res.Reason, "rolled back") {
		t.Fatalf("result %+v, want refused record_failed naming the rollback (#334 audit 2026-10-14: a daemon-side record failure is not the agent's refusal)", res)
	}
	if f.c.Registry.GetWorkflow("hermes--comms--reply-mail") == nil || f.c.Registry.GetProject("hermes--comms") == nil {
		t.Fatal("the project or its workflow is gone although the removal was not recorded")
	}
	p, err := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal %+v, %v; want ROLLED_BACK", p, err)
	}
	notBusy(t, f, "after the rolled-back transition change")
}

// Double failure (§23.4, review 9e97 F3): the record fails and so does the
// rollback. The agent is told the change stays applied; nothing pretends it
// was undone.
func TestAgentAdminLedger_DoubleFailureSaysTheChangeStayed(t *testing.T) {
	f := financeWithWorkflow(t)
	ctx := context.Background()
	f.svc.grants = &failingGrants{AgentGrantRepository: f.svc.grants, deleteReach: errors.New("injected: reach delete failed")}
	f.svc.rollback = func(context.Context, string) error { return errors.New("injected: rollback failed") }
	raw, _ := json.Marshal(agentadmin.RemoveInput{Kind: "workflow", ID: "finance--spend"})
	res, err := f.svc.Do(ctx, f.key, agentadmin.VerbRemove, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != agentadmin.EffectRefused || res.Rule != agentadmin.RuleRecordFailed || !strings.Contains(res.Reason, "rolling it back failed") {
		t.Fatalf("result %+v, want refused saying the rollback failed", res)
	}
	p, err := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal %+v, %v; want APPLIED (the rollback did not happen)", p, err)
	}
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #373 (found at: review, agent-administered design §23.9): the
// rollback of a change whose record failed ran on the request's context, so
// a client that disconnected between the failed record and the rollback
// cancelled the rollback and left the change applied and unrecorded, logged
// only at ERROR, with no metric and nothing the doctor could see.

// disconnectingGrants fails the reach delete and, in the same instant,
// cancels the request's context: the client went away right as the record
// failed, which is the seam between the failed record and the rollback.
type disconnectingGrants struct {
	persistence.AgentGrantRepository
	disconnect context.CancelFunc
}

func (g *disconnectingGrants) DeleteWorkflowReach(context.Context, string) error {
	g.disconnect()
	return errors.New("injected: reach delete failed")
}

func removeSpend(ctx context.Context, t *testing.T, f *agentAdminFixture) AgentAdminResult {
	t.Helper()
	raw, _ := json.Marshal(agentadmin.RemoveInput{Kind: "workflow", ID: "finance--spend"})
	res, err := f.svc.Do(ctx, f.key, agentadmin.VerbRemove, raw)
	if err != nil {
		t.Fatalf("an error instead of a refusal: %v", err)
	}
	return res
}

// The defect: a disconnect between the failed record and the rollback used
// to cancel the rollback, so the workflow's removal stayed applied (the
// proposal APPLIED, the file gone) with its reach row still standing.
func TestAgentAdminRollback_ClientDisconnectAfterFailedRecordStillRollsBack(t *testing.T) {
	f := financeWithWorkflow(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.grants = &disconnectingGrants{AgentGrantRepository: f.svc.grants, disconnect: cancel}

	res := removeSpend(ctx, t, f)
	if res.Effect != agentadmin.EffectRefused || res.Rule != agentadmin.RuleRecordFailed || !strings.Contains(res.Reason, "was rolled back") {
		t.Fatalf("result %+v, want refused record_failed saying it was rolled back (#373: the disconnect cancelled the rollback)", res)
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal %+v, %v; want ROLLED_BACK (#373: applied and unrecorded after a disconnect)", p, err)
	}
	if f.c.Registry.GetWorkflow("hermes--finance--spend") == nil {
		t.Fatal("the workflow is gone although its removal was not recorded")
	}
}

// R2: a verify miss whose caller's context is cancelled during the verify
// (the client went away while the reload ran) still rolls the change back.
func TestAgentAdminRollback_ClientDisconnectDuringVerifyStillRollsBack(t *testing.T) {
	f := financeWithWorkflow(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.verify = func(agentadmin.Change) string {
		cancel()
		return "spend"
	}

	res := removeSpend(ctx, t, f)
	if res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "the change was rolled back") {
		t.Fatalf("result %+v, want refused saying the change was rolled back (#373: the disconnect cancelled the verify-miss rollback)", res)
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal %+v, %v; want ROLLED_BACK", p, err)
	}
}

// disconnectOnApprove cancels the request's context once the change is
// approved, the step just before the apply.
type disconnectOnApprove struct {
	persistence.ProposalRepository
	disconnect context.CancelFunc
}

func (r disconnectOnApprove) SetStatus(ctx context.Context, id, status, actor string) error {
	err := r.ProposalRepository.SetStatus(ctx, id, status, actor)
	if status == persistence.ProposalStatusApproved {
		r.disconnect()
	}
	return err
}

// R3: an apply that fails because the client went away still marks its
// proposal REJECTED; on the request's context the write failed and left an
// un-applied agent proposal APPROVED.
func TestAgentAdminRollback_ClientDisconnectBeforeApplyStillRejects(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.proposals = disconnectOnApprove{ProposalRepository: f.svc.proposals, disconnect: cancel}
	raw, _ := json.Marshal(agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	res, err := f.svc.Do(ctx, f.key, agentadmin.VerbCreateProject, raw)
	if err != nil {
		t.Fatalf("an error instead of a refusal: %v", err)
	}
	if res.Effect != agentadmin.EffectRefused || res.ChangeID == "" {
		t.Fatalf("result %+v, want a refused change", res)
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRejected {
		status := ""
		if p != nil {
			status = p.Status
		}
		t.Fatalf("proposal status %q, %v; want REJECTED (#373: the compensation ran on the cancelled request context)", status, err)
	}
}

// A rollback that meets another apply holding the config writer lock
// (ErrApplyInProgress from TryLock) is retried inside its bound instead of
// leaving the change applied.
func TestAgentAdminRollback_RetriesWhileAnotherApplyHoldsTheLock(t *testing.T) {
	f := financeWithWorkflow(t)
	f.svc.grants = &failingGrants{AgentGrantRepository: f.svc.grants, deleteReach: errors.New("injected: reach delete failed")}
	attempts := 0
	f.svc.rollback = func(ctx context.Context, id string) error {
		attempts++
		if attempts <= 2 {
			return controlplane.ErrApplyInProgress
		}
		return f.svc.engine.Rollback(ctx, id)
	}

	res := removeSpend(context.Background(), t, f)
	if !strings.Contains(res.Reason, "was rolled back") || attempts != 3 {
		t.Fatalf("result %+v after %d attempt(s); want rolled back on the third", res, attempts)
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal %+v, %v; want ROLLED_BACK", p, err)
	}
}

// A rollback that fails for good is counted, recorded durably, and seen by
// the doctor's source until the operator rolls the change back.
func TestAgentAdminRollback_DoubleFailureIsCountedRecordedAndSeenByTheDoctor(t *testing.T) {
	f := financeWithWorkflow(t)
	ctx := context.Background()
	f.svc.grants = &failingGrants{AgentGrantRepository: f.svc.grants, deleteReach: errors.New("injected: reach delete failed")}
	f.svc.rollback = func(context.Context, string) error { return errors.New("injected: rollback failed") }
	failed := agentAdminRollbacks.WithLabelValues("record", "failed")
	before := testutil.ToFloat64(failed)

	res := removeSpend(ctx, t, f)
	if !strings.Contains(res.Reason, "stays applied") {
		t.Fatalf("result %+v, want the stays-applied answer", res)
	}
	if got := testutil.ToFloat64(failed) - before; got != 1 {
		t.Fatalf("vornik_agent_admin_rollbacks_total{record,failed} moved by %v, want 1", got)
	}
	rows, err := f.c.repos.AdminAudit.List(ctx, persistence.AdminAuditFilter{Action: agentRollbackFailedAction, PageSize: 10})
	if err != nil || len(rows) != 1 || rows[0].Target != res.ChangeID || rows[0].Principal != "agent:hermes" || rows[0].Source != "daemon" {
		t.Fatalf("audit rows %+v, %v; want one agent_change.rollback_failed row for %s", rows, err, res.ChangeID)
	}
	if !strings.Contains(rows[0].After, `"stage":"record"`) || !strings.Contains(rows[0].After, `"verb":"remove"`) {
		t.Fatalf("audit after %q lacks verb and stage", rows[0].After)
	}

	rep, err := f.c.agentChangesStayingApplied(ctx)
	if err != nil || rep.Examined != 1 || len(rep.StillApplied) != 1 || rep.StillApplied[0].ProposalID != res.ChangeID {
		t.Fatalf("doctor source %+v, %v; want the one change still applied", rep, err)
	}

	// The operator rolls it back: the doctor sees it resolved.
	if err := f.svc.engine.Rollback(ctx, res.ChangeID); err != nil {
		t.Fatal(err)
	}
	rep, err = f.c.agentChangesStayingApplied(ctx)
	if err != nil || rep.Examined != 1 || len(rep.StillApplied) != 0 || rep.Resolved != 1 {
		t.Fatalf("doctor source %+v, %v; want it resolved", rep, err)
	}
}

package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #795 (production, 2026-11-16 23:42 and 2026-11-17 09:13; design
// part 22 §22.2.16c): the phone's approval ran the change on the HTTP
// request's context. The client left during the reload, the fence check
// before the ledger commit failed on the cancelled context, the journal row
// stayed VERIFYING with the file on disk, and applyAndVerify rejected the
// proposal. The tick's retry then answered "the change is rejected" and the
// workflow stayed unapproved.

// widening795 files an approval-gated change with no account transition, as
// the incident's workflow widening was: a sixth project over the fixture's $10
// ceiling. It returns the request id and the proposal id.
func (f *agentAdminFixture) widening795() (string, string) {
	f.t.Helper()
	f.fill("aa", "bb", "cc", "dd", "ee")
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	if res.Effect != agentadmin.EffectAwaiting {
		f.t.Fatalf("fixture did not file a widening change: %+v", res)
	}
	return filepath.Base(res.ApprovalURL), res.ChangeID
}

// connection795 files an approval-gated change that carries an account
// transition (a new MCP server).
func (f *agentAdminFixture) connection795() (string, string) {
	f.t.Helper()
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "extra", URL: "https://extra.example/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "EXTRA"}})
	if res.Effect != agentadmin.EffectAwaiting {
		f.t.Fatalf("fixture did not file a connection change: %+v", res)
	}
	return filepath.Base(res.ApprovalURL), res.ChangeID
}

func (f *agentAdminFixture) proposalStatus795(id string) string {
	f.t.Helper()
	p, err := f.svc.proposals.GetByID(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return p.Status
}

func (f *agentAdminFixture) openJournal795(id string) *persistence.ConfigApplyJournal {
	f.t.Helper()
	row, err := f.svc.engine.Journal.OpenForProposal(context.Background(), id)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

// #795: the phone's request is cancelled inside the reload; the approved
// change still lands in full.
func TestApprovedApply_CancelledRequestStillApplies_795(t *testing.T) {
	f := newAgentAdminFixture(t)
	reqID, propID := f.widening795()
	req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), reqID)
	if err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reload := f.svc.engine.Reload
	f.svc.engine.Reload = func() error {
		err := reload()
		cancel() // the client leaves while the reload runs
		return err
	}
	_ = f.c.approverDeviceService().Decide(reqCtx, f.device, reqID, req.RenderedSHA256, true)
	f.svc.engine.Reload = reload

	if st := f.proposalStatus795(propID); st != persistence.ProposalStatusApplied {
		t.Fatalf("proposal is %s after a cancelled request, want %s", st, persistence.ProposalStatusApplied)
	}
	if row := f.openJournal795(propID); row != nil {
		t.Fatalf("journal row %s left open in %s", row.ID, row.State)
	}
	if f.c.Registry.GetProject("hermes--xa") == nil {
		t.Fatal("the approved project did not load")
	}
	got, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), reqID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppliedAt == nil || got.ApplyError != "" {
		t.Fatalf("request not recorded applied: applied_at=%v error=%q", got.AppliedAt, got.ApplyError)
	}
}

// #795: an apply that fails with its journal row left open (here the fence
// closes at the ledger commit) keeps the proposal APPROVED and is transient;
// the next attempt resumes the row and applies.
func TestApprovedApply_OpenJournalKeepsProposalApproved_795(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	reqID, propID := f.widening795()
	reload := f.svc.engine.Reload
	gate := f.svc.engine.LeaderGate
	f.svc.engine.Reload = func() error {
		err := reload()
		f.svc.engine.LeaderGate = failingEpoch795{} // the fence closes after the writes
		return err
	}
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.c.repos.ApproverDevices.Decide(ctx, reqID, req.RenderedSHA256, f.device.ID, true, req.CreatedAt); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.applyApproved(ctx, *row)
	if err == nil || errors.Is(err, approverdevice.ErrPermanent) {
		t.Fatalf("a fenced apply with its row open answered %v, want a transient error", err)
	}
	if st := f.proposalStatus795(propID); st != persistence.ProposalStatusApproved {
		t.Fatalf("proposal is %s with its journal row open, want %s", st, persistence.ProposalStatusApproved)
	}
	if f.openJournal795(propID) == nil {
		t.Fatal("fixture did not leave the journal row open")
	}
	f.svc.engine.Reload = reload
	f.svc.engine.LeaderGate = gate
	if err = f.svc.applyApproved(ctx, *row); err != nil {
		t.Fatalf("the retry did not resume the row: %v", err)
	}
	if st := f.proposalStatus795(propID); st != persistence.ProposalStatusApplied {
		t.Fatalf("proposal is %s after the retry, want %s", st, persistence.ProposalStatusApplied)
	}
	if r := f.openJournal795(propID); r != nil {
		t.Fatalf("journal row %s still open after the retry", r.ID)
	}
}

// #795, the limit of §22.2.16c item 3: a change with an account transition
// whose row is left open keeps its namespace busy for controlled recovery
// (§22.2.16), so a retry cannot begin; the refusal stays final, but the
// proposal is no longer rejected.
func TestApprovedApply_OpenJournalWithAccountTransitionIsFinal_795(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	reqID, propID := f.connection795()
	reload := f.svc.engine.Reload
	f.svc.engine.Reload = func() error {
		err := reload()
		f.svc.engine.LeaderGate = failingEpoch795{}
		return err
	}
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.c.repos.ApproverDevices.Decide(ctx, reqID, req.RenderedSHA256, f.device.ID, true, req.CreatedAt); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.applyApproved(ctx, *row); !errors.Is(err, approverdevice.ErrPermanent) {
		t.Fatalf("an open row behind an account transition answered %v, want ErrPermanent", err)
	}
	if st := f.proposalStatus795(propID); st != persistence.ProposalStatusApproved {
		t.Fatalf("proposal is %s with its journal row open, want %s", st, persistence.ProposalStatusApproved)
	}
}

// failingEpoch795 is a leader gate whose epoch read fails, as it did on the
// cancelled context.
type failingEpoch795 struct{}

func (failingEpoch795) VerifyEpoch(context.Context) (bool, int64, error) {
	return false, 0, context.Canceled
}

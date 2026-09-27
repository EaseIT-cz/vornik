package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Config-drift slice E (the pre-apply snapshot, decided 2026-09-24): a `d`
// hunk an APPLIED remove_step / reorder_steps explains is not a missed fix —
// the operator approved exactly that deletion — so it is counted, not listed.
// Before this, every approved removal of a shipped step read as a template fix
// the deployment "lacks", forever.

type fakeLedger struct {
	rows []*persistence.WorkflowProposal
	err  error
	got  persistence.WorkflowProposalFilter
}

func (f *fakeLedger) List(_ context.Context, filter persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	f.got = filter
	if f.err != nil {
		return nil, f.err
	}
	var out []*persistence.WorkflowProposal
	for _, p := range f.rows {
		if p.WorkflowID == filter.WorkflowID {
			out = append(out, p)
		}
	}
	return out, nil
}

const (
	ledgerStepA = "  a:\n    role: coder\n    retry:\n      max: 3\n"
	ledgerStepB = "  b:\n    role: reviewer\n"
)

func ledgerTree(t *testing.T, tmpl, deployed string) string {
	return driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/workflows/w.md": tmpl, "workflows/w.md": deployed,
	})
}

func appliedRemoval(pre, post string) *persistence.WorkflowProposal {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	return &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "w", Status: persistence.WorkflowProposalStatusApplied,
		Kind: persistence.WorkflowProposalKindRemoveStep, ProposalYAML: post, PreApplyYAML: pre, AppliedAt: &at,
	}
}

func TestConfigTemplateDrift_AnApprovedRemovalIsExplained(t *testing.T) {
	tmpl, deployed := "steps:\n"+ledgerStepA+ledgerStepB, "steps:\n"+ledgerStepB
	h := driftHandler(ledgerTree(t, tmpl, deployed), "1111111aaaaa")
	led := &fakeLedger{rows: []*persistence.WorkflowProposal{appliedRemoval(tmpl, deployed)}}
	h.SetWorkflowProposals(led)
	got := h.checkConfigTemplateDrift()
	if got.Status != "OK" || !strings.Contains(got.Message, "1 hunk(s) explained by applied proposals") {
		t.Fatalf("an approved removal was not explained: %s %q %v", got.Status, got.Message, got.Items)
	}
	f := led.got
	if len(f.Statuses) != 1 || f.Statuses[0] != persistence.WorkflowProposalStatusApplied || len(f.Kinds) != 2 {
		t.Errorf("the ledger was not asked for applied remove/reorder only: %+v", f)
	}
}

func TestConfigTemplateDrift_ANeverTakenFixBesideAnApprovalIsReported(t *testing.T) {
	pre, post := "steps:\n"+ledgerStepA+ledgerStepB, "steps:\n"+ledgerStepB
	tmpl := "steps:\n" + ledgerStepB + "    recovery: true\n"
	h := driftHandler(ledgerTree(t, tmpl, post), "1111111aaaaa")
	h.SetWorkflowProposals(&fakeLedger{rows: []*persistence.WorkflowProposal{appliedRemoval(pre, post)}})
	got := h.checkConfigTemplateDrift()
	if got.Status != "WARNING" || !strings.Contains(strings.Join(got.Items, "\n"), "recovery: true") {
		t.Fatalf("a never-taken fix was hidden by an unrelated approval: %s %v", got.Status, got.Items)
	}
}

func TestConfigTemplateDrift_AnApprovalWithoutASnapshotSaysSo(t *testing.T) {
	tmpl, deployed := "steps:\n"+ledgerStepA+ledgerStepB, "steps:\n"+ledgerStepB
	h := driftHandler(ledgerTree(t, tmpl, deployed), "1111111aaaaa")
	h.SetWorkflowProposals(&fakeLedger{rows: []*persistence.WorkflowProposal{appliedRemoval("", deployed)}})
	got := h.checkConfigTemplateDrift()
	joined := strings.Join(got.Items, "\n")
	if got.Status != "WARNING" || !strings.Contains(joined, "an applied remove_step of 2026-09-01 may explain this hunk; its pre-apply file was not recorded") {
		t.Fatalf("a pre-197 approval was neither applied nor named: %s %v", got.Status, got.Items)
	}
}

func TestConfigTemplateDrift_AnUnreachableLedgerRendersEveryHunk(t *testing.T) {
	tmpl, deployed := "steps:\n"+ledgerStepA+ledgerStepB, "steps:\n"+ledgerStepB
	for name, led := range map[string]workflowProposalLedger{"error": &fakeLedger{err: errors.New("db down")}, "unwired": nil} {
		h := driftHandler(ledgerTree(t, tmpl, deployed), "1111111aaaaa")
		if led != nil {
			h.SetWorkflowProposals(led)
		}
		got := h.checkConfigTemplateDrift()
		if got.Status != "WARNING" || !strings.Contains(strings.Join(got.Items, "\n"), "retry:") {
			t.Fatalf("%s: the hunk was not rendered: %s %v", name, got.Status, got.Items)
		}
		if !strings.Contains(got.Message, "applied-proposal exemption could not be evaluated") {
			t.Errorf("%s: the row does not say the exemption was not evaluated: %q", name, got.Message)
		}
	}
}

// A clean tree says nothing about the ledger: there was nothing to explain.
func TestConfigTemplateDrift_NoHunksNoLedgerSentence(t *testing.T) {
	h := driftHandler(ledgerTree(t, "x\n", "x\n"), "1111111aaaaa")
	if got := h.checkConfigTemplateDrift(); strings.Contains(got.Message, "exemption") {
		t.Fatalf("a clean tree mentions the exemption: %q", got.Message)
	}
}

// Two approved removals on one workflow, each explaining one hunk: the union
// reaches the row end to end (round 17 implementation review F5).
func TestConfigTemplateDrift_TwoApprovalsExplainTheirOwnHunks(t *testing.T) {
	stepC := "  c:\n    role: tester\n"
	tmpl := "steps:\n" + ledgerStepA + ledgerStepB + stepC
	mid := "steps:\n" + ledgerStepB + stepC
	deployed := "steps:\n" + ledgerStepB
	first := appliedRemoval(tmpl, mid)
	second := appliedRemoval(mid, deployed)
	second.ID = "wpr-2"
	h := driftHandler(ledgerTree(t, tmpl, deployed), "1111111aaaaa")
	h.SetWorkflowProposals(&fakeLedger{rows: []*persistence.WorkflowProposal{second, first}})
	got := h.checkConfigTemplateDrift()
	if got.Status != "OK" || !strings.Contains(got.Message, "2 hunk(s) explained by applied proposals") {
		t.Fatalf("two approvals did not combine: %s %q %v", got.Status, got.Message, got.Items)
	}
	// Either one alone leaves the other's hunk reported.
	h.SetWorkflowProposals(&fakeLedger{rows: []*persistence.WorkflowProposal{first}})
	if got := h.checkConfigTemplateDrift(); got.Status != "WARNING" {
		t.Fatalf("one approval explained both hunks: %s %q", got.Status, got.Message)
	}
}

package workflowhealing

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #921 (bench arm C, 2026-11-25): a candidate whose trial failed kept
// its proposal pending, and the one-pending-proposal-per-workflow cap then
// refused every later candidate ("workflow already has a pending
// proposal"). Rejecting a candidate now rejects its pending proposal, so the
// workflow can be proposed for again.
func TestReject_FreesThePendingProposal_921(t *testing.T) {
	cr := newFakeCandidateRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialFailed, "wpr_1")
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})

	got, err := NewPromoter(cr, pr, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.Status != persistence.HealingCandidateRejected {
		t.Fatalf("candidate status = %s, want rejected", got.Status)
	}
	p, _ := pr.Get(context.Background(), "wpr_1")
	if p.Status != persistence.WorkflowProposalStatusRejected {
		t.Fatalf("proposal status = %s, want rejected (it held the workflow's one pending slot)", p.Status)
	}
}

// A proposal that is no longer pending is left alone, and a missing one does
// not fail the rejection.
func TestReject_LeavesADecidedProposalAlone_921(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status persistence.WorkflowProposalStatus
		seed   bool
	}{
		{"approved", persistence.WorkflowProposalStatusApproved, true},
		{"missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := newFakeCandidateRepo()
			seedPromoCandidate(t, cr, persistence.HealingCandidateTrialFailed, "wpr_1")
			pr := newFakeProposalRepo()
			if tc.seed {
				pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: tc.status})
			}
			if _, err := NewPromoter(cr, pr, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test"); err != nil {
				t.Fatalf("Reject: %v", err)
			}
			if len(pr.decided) != 0 {
				t.Fatalf("decided %v, want nothing", pr.decided)
			}
		})
	}
}

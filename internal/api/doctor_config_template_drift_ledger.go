package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/configdrift"
	"vornik.io/vornik/internal/persistence"
)

// workflowProposalLedger is the slice of the proposal repository the drift
// row reads: the applied remove_step / reorder_steps proposals of a workflow.
type workflowProposalLedger interface {
	List(ctx context.Context, filter persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error)
}

// SetWorkflowProposals wires the applied-proposal ledger into
// config_template_drift (drift design, slice E). Unwired, every hunk renders
// and the row says the exemption could not be evaluated.
func (h *DoctorHandlers) SetWorkflowProposals(l workflowProposalLedger) {
	h.workflowProposals = l
}

// ledgerPageSize bounds one workflow's applied remove/reorder proposals.
// Fewer changes can only exempt fewer hunks, so a truncated page errs toward
// reporting — and the row says it was truncated.
const ledgerPageSize = 200

// ledgerTimeout bounds ONE file's ledger read, so a slow read cannot use up
// the budget of the files after it.
const ledgerTimeout = 5 * time.Second

// driftLedger evaluates the slice E exemption for the tunable workflow files
// of one doctor run.
type driftLedger struct {
	l         workflowProposalLedger
	explained int
	// unevaluated names why a file with hard hunks could not be checked
	// against the ledger (unwired, a failed query); empty when it was.
	unevaluated string
	truncated   bool
}

// ledgerEval is one file's standing: the hunk indices an approved change
// explains, and the applied proposals that may explain a hunk but recorded no
// pre-apply file.
type ledgerEval struct {
	exempt     map[int]bool
	unrecorded []string
}

// workflowIDOf maps a deployed-tree path to its workflow id; ok is false for
// any file the ledger cannot speak about.
func workflowIDOf(rel string) (string, bool) {
	id, found := strings.CutPrefix(rel, "workflows/")
	if !found || !strings.HasSuffix(id, ".md") || strings.Contains(id, "/") {
		return "", false
	}
	return strings.TrimSuffix(id, ".md"), true
}

// eval returns ff's exemptions. It consults the ledger only for a workflow
// file with hard hunks, so a clean tree never says anything about it.
func (dl *driftLedger) eval(ff configdrift.FileFinding) ledgerEval {
	id, ok := workflowIDOf(ff.Rel)
	if !ok || len(ff.HardHunks()) == 0 {
		return ledgerEval{}
	}
	if dl.l == nil {
		dl.unevaluated = "the proposal ledger is not wired"
		return ledgerEval{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	rows, err := dl.l.List(ctx, persistence.WorkflowProposalFilter{
		WorkflowID: id,
		Statuses:   []persistence.WorkflowProposalStatus{persistence.WorkflowProposalStatusApplied},
		Kinds:      persistence.PreApplySnapshotKinds(),
		PageSize:   ledgerPageSize,
	})
	if err != nil {
		dl.unevaluated = "the proposal ledger could not be read (" + err.Error() + ")"
		return ledgerEval{}
	}
	if len(rows) >= ledgerPageSize {
		dl.truncated = true
	}
	var ev ledgerEval
	var changes []configdrift.ApprovedChange
	for _, p := range rows {
		if p.PreApplyYAML == "" {
			date := "an unknown date"
			if p.AppliedAt != nil {
				date = p.AppliedAt.UTC().Format("2006-01-02")
			}
			ev.unrecorded = append(ev.unrecorded, fmt.Sprintf("an applied %s of %s", p.Kind, date))
			continue
		}
		changes = append(changes, configdrift.ApprovedChange{PreApply: []byte(p.PreApplyYAML), PostApply: []byte(p.ProposalYAML)})
	}
	ev.exempt = ff.ExemptHunks(changes)
	return ev
}

// denominator is the ledger's clause in the row's message, if any.
func (dl *driftLedger) denominator() string {
	var out string
	if dl.explained > 0 {
		out += fmt.Sprintf("; %d hunk(s) explained by applied proposals", dl.explained)
	}
	if dl.unevaluated != "" {
		out += "; applied-proposal exemption could not be evaluated: " + dl.unevaluated + ", so every hunk is shown"
	}
	if dl.truncated {
		out += fmt.Sprintf("; a workflow has more than %d applied remove/reorder proposals, only the newest were consulted", ledgerPageSize)
	}
	return out
}

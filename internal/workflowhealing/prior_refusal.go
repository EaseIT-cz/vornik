package workflowhealing

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The pre-trial refusal codes. preTrialRefusals begins every refusal reason
// with one of these, and PriorRefusal selects refusal reasons by them, so the
// trial and the producer cannot disagree on what a refusal is.
const (
	RefusalCodeParse          = "parse_error"
	RefusalCodeValidator      = "validator_error"
	RefusalCodeInertTimeout   = "step_timeout_exceeds_wall_clock"
	priorRefusalWindow        = 30 * 24 * time.Hour
	priorRefusalCandidatePage = 20
	// Bounds on what reaches the assistant's intent.
	maxPriorRefusalReasons     = 5
	maxPriorRefusalReasonRunes = 300
)

// RefusalCodes returns the declared pre-trial refusal codes.
func RefusalCodes() []string {
	return []string{RefusalCodeParse, RefusalCodeValidator, RefusalCodeInertTimeout}
}

// refusalReason is the one way a refusal reason is written: "<code>: <detail>".
// isRefusalReason reads that prefix, so writing it anywhere else by hand is how
// the producer would stop recognising a refusal (review a591 N5).
func refusalReason(code, detail string) string { return code + ": " + detail }

func isRefusalReason(r string) bool {
	for _, c := range RefusalCodes() {
		if strings.HasPrefix(r, c+": ") {
			return true
		}
	}
	return false
}

// PriorRefusalInfo is the most recent pre-trial refusal for a workflow.
type PriorRefusalInfo struct {
	CandidateID string
	RefusedAt   time.Time
	// Reasons are the refusal reasons only, flattened and bounded.
	Reasons []string
}

// PriorRefusal finds the most recent pre-trial refusal for (project, workflow)
// within the last 30 days, so the next candidate's producer can be told not to
// repeat it (self-healing genome design, "The producer hears the last
// refusal", 2026-09-24). It walks the workflow's newest candidates of ANY
// status — a refused candidate an operator later rejected still carries its
// trial — and returns the first whose newest trial carries refusal reasons.
// A replay-gate failure is not a refusal and is skipped. nil, nil means none;
// an error means the lookup failed, which is not the same thing.
func PriorRefusal(ctx context.Context, cands persistence.WorkflowHealingCandidateRepository, trials persistence.WorkflowHealingTrialRepository, projectID, workflowID string, now time.Time) (*PriorRefusalInfo, error) {
	if cands == nil || trials == nil {
		return nil, nil
	}
	rows, err := cands.List(ctx, persistence.HealingCandidateListFilter{
		ProjectID: projectID, WorkflowID: workflowID, PageSize: priorRefusalCandidatePage,
	})
	if err != nil {
		return nil, fmt.Errorf("workflowhealing: list candidates: %w", err)
	}
	// Newest first, by construction here rather than by trusting each
	// backend's default order (review b134 N2).
	rows = append([]*persistence.HealingCandidate(nil), rows...)
	sort.SliceStable(rows, func(i, j int) bool { return candidateTime(rows[i]).After(candidateTime(rows[j])) })
	cutoff := now.Add(-priorRefusalWindow)
	for _, c := range rows {
		if c == nil {
			continue
		}
		trs, err := trials.ListByCandidate(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("workflowhealing: list trials of %s: %w", c.ID, err)
		}
		// Every trial of the candidate, newest first — a candidate re-trialled
		// after a refusal (trial_failed is not terminal) keeps the refusal in
		// an older trial (review a6ff F4).
		if info := newestRefusalTrial(c.ID, trs, cutoff); info != nil {
			return info, nil
		}
	}
	return nil, nil
}

// newestRefusalTrial returns the newest failed trial within the window that
// carries refusal reasons, or nil.
//
// Only FINISHED trials count: the window is on finished_at, and a failed
// trial with no finish time is anomalous, not a refusal to report.
func newestRefusalTrial(candidateID string, trs []*persistence.HealingTrial, cutoff time.Time) *PriorRefusalInfo {
	var finished []*persistence.HealingTrial
	for _, tr := range trs {
		if tr != nil && tr.Verdict == persistence.HealingTrialFailed && tr.FinishedAt != nil {
			finished = append(finished, tr)
		}
	}
	sort.SliceStable(finished, func(i, j int) bool { return finished[i].FinishedAt.After(*finished[j].FinishedAt) })
	for _, tr := range finished {
		at := *tr.FinishedAt
		if at.Before(cutoff) {
			continue
		}
		var sc HealingScorecard
		if json.Unmarshal([]byte(tr.Scorecard), &sc) != nil {
			continue
		}
		// Timeout refusals first, then the rest, each in scorecard order, so
		// the five-reason cap can never drop the reason that carries the
		// maxWallClock rule (review a591 N4).
		var timeouts, others []string
		for _, r := range sc.Reasons {
			switch {
			case strings.HasPrefix(r, RefusalCodeInertTimeout+": "):
				timeouts = append(timeouts, r)
			case isRefusalReason(r):
				others = append(others, r)
			}
		}
		var reasons []string
		for _, r := range append(timeouts, others...) {
			if len(reasons) == maxPriorRefusalReasons {
				break
			}
			reasons = append(reasons, boundReason(r))
		}
		if len(reasons) > 0 {
			return &PriorRefusalInfo{CandidateID: candidateID, RefusedAt: at, Reasons: reasons}
		}
	}
	return nil
}

// candidateTime orders candidates; a zero CreatedAt sorts last.
func candidateTime(c *persistence.HealingCandidate) time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.CreatedAt
}

// boundReason flattens newlines and cuts a reason to the rune bound: the
// reasons embed values a model wrote into the prior candidate.
func boundReason(r string) string {
	r = strings.Join(strings.Fields(r), " ")
	if rs := []rune(r); len(rs) > maxPriorRefusalReasonRunes {
		r = string(rs[:maxPriorRefusalReasonRunes-1]) + "…"
	}
	return r
}

// Constraint renders the refusal as the sentence appended to the producer's
// intent.
func (p *PriorRefusalInfo) Constraint() string {
	if p == nil || len(p.Reasons) == 0 {
		return ""
	}
	// The timeout rule only when a reason IS a timeout refusal — a parse or
	// validator refusal must not be handed guidance about something else
	// (review b134 N1).
	rule := "Do not repeat these."
	for _, r := range p.Reasons {
		if strings.HasPrefix(r, RefusalCodeInertTimeout+": ") {
			rule = "Do not repeat these; keep every step timeout within the workflow's maxWallClock."
			break
		}
	}
	return fmt.Sprintf("A previous candidate for this workflow (%s, refused %s) was refused before trial: %s. %s",
		p.CandidateID, p.RefusedAt.UTC().Format("2006-01-02"), strings.Join(p.Reasons, "; "), rule)
}

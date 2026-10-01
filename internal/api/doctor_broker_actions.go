package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SetBrokerActionRepository wires the broker-action store for the
// stuck_broker_actions check. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetBrokerActionRepository(repo persistence.BrokerActionRepository) {
	h.brokerActions = repo
}

// checkStuckBrokerActions reports broker write actions no one is finishing
// (design 2026-09-29 §5.4): executing or unknown for over 15 minutes, which
// only an operator can resolve, and approved for over 5 minutes, which means
// the action worker is not running. Read-only: nothing here may decide
// whether a write happened, so there is no --fix.
func (h *DoctorHandlers) checkStuckBrokerActions(ctx context.Context) DoctorCheck {
	const name = "stuck_broker_actions"
	if h.brokerActions == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "broker-action store not wired"}
	}
	const perStatus = 500
	now := time.Now().UTC()
	examined := 0
	var capped []string
	var items []string
	for _, status := range []string{persistence.BrokerActionApproved, persistence.BrokerActionExecuting, persistence.BrokerActionUnknown} {
		rows, err := h.brokerActions.ListByStatus(ctx, "", status, perStatus)
		if err != nil {
			return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("list %s: %v", status, err)}
		}
		examined += len(rows)
		if len(rows) == perStatus {
			capped = append(capped, status)
		}
		for _, a := range rows {
			if !persistence.BrokerActionIsStuck(a, now) {
				continue
			}
			items = append(items, stuckBrokerActionItem(a, now))
		}
	}
	scope := fmt.Sprintf("%d approved/executing/unknown examined", examined)
	// Per status, and "by creation" because that is ListByStatus's order
	// (review-20260930-1334 F5).
	if len(capped) > 0 {
		scope += fmt.Sprintf("; only the oldest %d (by creation) were read for %s, so more may be stuck",
			perStatus, strings.Join(capped, ", "))
	}
	if len(items) == 0 {
		return DoctorCheck{Name: name, Status: "OK",
			Message: "no stuck broker actions (" + scope + ")"}
	}
	return DoctorCheck{Name: name, Status: "WARNING",
		Message: fmt.Sprintf("%d broker actions are stuck (%s)", len(items), scope),
		Items:   items}
}

func stuckBrokerActionItem(a *persistence.BrokerAction, now time.Time) string {
	age := now.Sub(persistence.BrokerActionAgeSince(a)).Truncate(time.Minute)
	head := fmt.Sprintf("%s (project=%s, task=%s, tool=%s, status=%s, age=%s)", a.ActionID, a.ProjectID, a.TaskID, a.Tool, a.Status, age)
	if a.Status == persistence.BrokerActionApproved {
		return head + ": approved but not executing; the action worker is not running (restart the daemon)"
	}
	return head + ": may or may not have been sent; check the vendor, then vornikctl broker-action resolve " +
		a.ActionID + " --executed|--failed"
}

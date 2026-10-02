package api

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// SetApproverDevices wires the approver_devices check (agent-administered
// Vornik design §9.2): the device store and whether pairings and approval
// requests are pushed (§9.3).
func (h *DoctorHandlers) SetApproverDevices(repo persistence.ApproverDeviceRepository, pushConfigured bool) {
	h.approverDevices, h.approverPush = repo, pushConfigured
}

// checkApproverDevices reports whether an agent could get anything approved.
// With no agent namespace there is nothing to approve. With one, zero active
// devices means every widening change and credential would wait forever, and
// no push channel means nobody is told a request is waiting.
func (h *DoctorHandlers) checkApproverDevices(ctx context.Context, _ bool) DoctorCheck {
	const name = "approver_devices"
	if h.approverDevices == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "approver device store not wired"}
	}
	n, err := h.approverDevices.CountActiveDevices(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not count approver devices: " + err.Error()}
	}
	agents := 0
	if h.agentSecrets != nil {
		nss, err := h.agentSecrets.ListNamespaces(ctx)
		if err != nil {
			return DoctorCheck{Name: name, Status: "WARNING", Message: "could not list agent namespaces: " + err.Error()}
		}
		agents = len(nss)
	}
	if agents == 0 {
		// doctor-vacuous: the check ran; with no agent namespace there is
		// nothing a device would be asked to approve.
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d active approver device(s); no agent namespaces yet", n)}
	}
	switch {
	case n == 0:
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"%d agent namespace(s) but no active approver device: nothing an agent asks for can be approved; pair one with vornikctl pair-device", agents)}
	case !h.approverPush:
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"%d active approver device(s) but no alert channel (steering_operator_alert): requests are not pushed, only shown at /ui/approve/", n)}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d active approver device(s); requests are pushed", n)}
}

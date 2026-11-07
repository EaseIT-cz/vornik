package api

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// SetApproverDevices wires the approver_devices check (agent-administered
// Vornik design §9.2): the device store and whether pairings and approval
// requests are pushed (§9.3).
func (h *DoctorHandlers) SetApproverDevices(repo persistence.ApproverDeviceRepository, pushConfigured bool) {
	h.approverDevices, h.approverPush = repo, pushConfigured
}

// SetApproverPush wires the Web Push subscriptions (GitHub #163, design part
// 09 §9.3a B.8), so the check counts devices with notifications on.
func (h *DoctorHandlers) SetApproverPush(repo persistence.ApproverPushRepository) {
	h.approverWebPush = repo
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
	if n == 0 {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"%d agent namespace(s) but no active approver device: nothing an agent asks for can be approved; pair one with vornikctl pair-device", agents)}
	}
	subs, failed := h.approverPushState(ctx)
	switch {
	case failed != "":
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"%d active approver device(s); the last notification to %s; open /ui/approve/push on that device to turn notifications off and on again", n, failed)}
	case !h.approverPush && subs == 0:
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"%d active approver device(s) but no alert channel (steering_operator_alert) and no device with notifications on: requests are not pushed, only shown at /ui/approve/", n)}
	case !h.approverPush:
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d active approver device(s); requests are pushed to %d device(s) with notifications on", n, subs)}
	case subs > 0:
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d active approver device(s); requests are pushed (alert channel, and %d device(s) with notifications on)", n, subs)}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d active approver device(s); requests are pushed", n)}
}

// approverPushState counts active Web Push subscriptions and names the
// devices whose last push failed ("Pixel failed 403"), or "" for none.
func (h *DoctorHandlers) approverPushState(ctx context.Context) (int, string) {
	if h.approverWebPush == nil {
		return 0, ""
	}
	subs, err := h.approverWebPush.ListActivePushSubscriptions(ctx)
	if err != nil {
		return 0, ""
	}
	var bad []string
	labels := map[string]string{}
	for _, s := range subs {
		if !strings.HasPrefix(s.LastStatus, "failed") {
			continue
		}
		if len(labels) == 0 {
			if devs, err := h.approverDevices.ListDevices(ctx); err == nil {
				for _, d := range devs {
					labels[d.ID] = d.Label
				}
			}
		}
		label := labels[s.DeviceID]
		if label == "" {
			label = s.DeviceID
		}
		bad = append(bad, fmt.Sprintf("%q %s", label, s.LastStatus))
	}
	return len(subs), strings.Join(bad, ", ")
}

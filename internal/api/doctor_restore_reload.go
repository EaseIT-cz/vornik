package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/controlplane"
)

// SetUnconfirmedReload wires control_plane_restore_reload to the daemon's
// shared reload guard (GitHub #859). Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetUnconfirmedReload(fn func() *controlplane.UnconfirmedReload) {
	h.unconfirmedReload = fn
}

// checkRestoreReload reports the daemon's last config reload that the control
// plane made and could not confirm: rejected, timed out, or refused because
// one was still running. A later reload the control plane makes, if it
// succeeds, clears it. The daemon may then be serving a different config
// than its files hold (control-plane apply/rollback design, amendment
// 2026-11-22).
func (h *DoctorHandlers) checkRestoreReload(_ context.Context) DoctorCheck {
	const name = "control_plane_restore_reload"
	if h.unconfirmedReload == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "control plane not wired"}
	}
	u := h.unconfirmedReload()
	if u == nil {
		return DoctorCheck{Name: name, Status: "OK", Message: "no unconfirmed config reload since the daemon started"}
	}
	remedy := "fix the file it names and run `vornikctl config reload`"
	if u.Kind != controlplane.ReloadRejected {
		remedy = "a config reload is stuck: restart the daemon"
	}
	return DoctorCheck{
		Name:   name,
		Status: "ERROR",
		Message: fmt.Sprintf("CRITICAL: the config reload for proposal %s was %s at %s (%s); the daemon may be serving "+
			"a different config than its files hold. Remedy: %s", u.Ref, u.Kind, u.At.UTC().Format(time.RFC3339), u.Error, remedy),
	}
}

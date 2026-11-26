package api

import (
	"fmt"
	"time"
)

// checkStartHeld is doctor's start_held (GitHub #586, deploy idle gate
// design §13.3): WARNING while held before the deadline, ERROR when
// overdue, OK otherwise. The message carries the reason, the time held, a
// malformed marker's error and a refused release's last error.
func (h *DoctorHandlers) checkStartHeld() DoctorCheck {
	const name = "start_held"
	if h.server == nil {
		return DoctorCheck{Name: name, Status: "OK", Message: "this daemon did not start held"}
	}
	st := h.server.HoldStatusNow()
	if !st.Held {
		msg := "not held"
		if st.LastError != "" {
			msg += "; last release error: " + st.LastError
		}
		return DoctorCheck{Name: name, Status: "OK", Message: msg}
	}
	held := h.server.drainClock().Sub(st.Info.Since).Round(time.Second)
	msg := fmt.Sprintf("held (hold %s: %s) for %s; deadline %s. No work runs until `vornikctl daemon release`",
		st.Info.HoldID, st.Info.Reason, held, st.Info.Deadline.UTC().Format(time.RFC3339))
	if st.Info.BaseFromStart {
		msg += "; the marker's created_at was missing or in the future, so the deadline counts from the process start"
	}
	var items []string
	if st.Info.MarkerErr != "" {
		items = append(items, "malformed marker (still held): "+st.Info.MarkerErr)
	}
	if st.LastError != "" {
		items = append(items, "last release refused: "+st.LastError)
	}
	if st.ReleaseFailed {
		items = append(items, "a release failed to start work; the process is exiting")
	}
	status := "WARNING"
	if st.Overdue {
		status = "ERROR"
		msg = "OVERDUE: " + msg
	}
	return DoctorCheck{Name: name, Status: status, Message: msg, Items: items}
}

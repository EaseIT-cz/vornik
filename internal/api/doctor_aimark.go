package api

import (
	"fmt"

	"vornik.io/vornik/internal/aimark"
)

// SetAIMarker wires the Art 50(2) marker behind checkAIOutputMarking (GitHub
// #117). A nil marker means marking is off, which the check reports; not
// calling this leaves the check SKIPPED.
func (h *DoctorHandlers) SetAIMarker(m *aimark.Marker) {
	if h == nil {
		return
	}
	h.aiMarker, h.aiMarkerSet = m, true
}

// checkAIOutputMarking reports whether generated output is marked (EU AI Act
// Art 50(2); Art 50 design §14.7, §14.8) and how many marks since start were
// attached but not recorded in the ledger, so exact-copy detection misses
// them (§14.7 F4).
func (h *DoctorHandlers) checkAIOutputMarking() DoctorCheck {
	const name = "ai_output_marking"
	switch {
	case h == nil || !h.aiMarkerSet:
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "AI output marking is not wired into the doctor"}
	case h.aiMarker == nil:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: "AI output marking (EU AI Act Art 50(2)) is off: the daemon has no secret-store key to sign with, so generated output is sent unmarked. Store any credential (which creates the key) and restart."}
	}
	if n := h.aiMarker.LedgerErrors(); n > 0 {
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: fmt.Sprintf("%d mark(s) attached to output since start but not recorded in ai_output_marks; the detection endpoint cannot find those outputs. Check the database.", n)}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: "AI output marking is on (key " + h.aiMarker.KID() + ")"}
}

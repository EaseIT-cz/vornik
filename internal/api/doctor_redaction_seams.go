package api

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/auditredact"
)

// checkRedactionSeams reports whether the content stores the redaction seam
// decorates — chat_audit_log, chat_system_prompts, step_prompts and
// task_messages — are scanned for secrets before they are persisted, and
// publishes the DENOMINATOR: how many writes reached each store since boot and
// how many of them were not examined.
//
// Until 2026-10-16 these decorators were simply absent when secrets.enabled was
// off or the detector failed to build (task_messages had none at all), so a
// deployment writing plaintext transcripts looked identical to one with
// nothing to redact. The decorators are now always installed and count every
// write; this check is the operator-facing half of that census, for an
// operator who has no Prometheus (chat-audit retention and redaction design
// §9.2.1, GitHub #395). tool_audit_redaction keeps tool_audit_log, whose
// census has a detect_only status these stores do not.
//
// Severity follows the census design: a control the operator asked for that
// is not running (detector_unavailable) is ERROR; a deployment choice
// (secrets_disabled, detector_nil) is WARNING; never evaluated is SKIPPED.
func (h *DoctorHandlers) checkRedactionSeams() DoctorCheck {
	const name = "redaction_seams"
	if !h.redactionSeamsKnown {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no redaction-seam snapshot captured, skipping"}
	}
	if len(h.redactionSeamStores) == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no content store is wired on this deployment, skipping"}
	}
	var census map[string]auditredact.SeamCount
	if h.redactionSeamCensus != nil {
		census = h.redactionSeamCensus()
	}
	counts := seamCensusLine(h.redactionSeamStores, census)
	stores := strings.Join(h.redactionSeamStores, ", ")

	switch h.redactionSeamReason {
	case auditredact.ReasonNone:
		return DoctorCheck{Name: name, Status: "OK",
			Message: fmt.Sprintf("%s rows are scanned before persist; since boot: %s", stores, counts)}
	case auditredact.ReasonSecretsDisabled:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: fmt.Sprintf("secret scanning is off (secrets.enabled=false), so %s rows are persisted "+
				"unscanned and may hold plaintext credentials at rest; since boot: %s; counted on "+
				"vornik_redaction_seam_rows_total{status=\"skipped\"}", stores, counts)}
	case auditredact.ReasonDetectorUnavailable:
		return DoctorCheck{Name: name, Status: "ERROR",
			Message: fmt.Sprintf("secrets.enabled=true but the secret detector failed to construct, so %s rows "+
				"are persisted unscanned — the daemon is not running a control this deployment asked for; "+
				"check secrets.patterns.custom for an invalid regex in the daemon log; since boot: %s", stores, counts)}
	default:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: fmt.Sprintf("%s rows are persisted with no detector (reason=%s); expected on CE and dev "+
				"deployments; since boot: %s", stores, h.redactionSeamReason, counts)}
	}
}

// seamCensusLine renders "<store> <total> rows (<redacted> redacted, <skipped>
// unscanned)" for every store, zero rows included: a store missing from the
// line would read as clean.
func seamCensusLine(stores []string, census map[string]auditredact.SeamCount) string {
	parts := make([]string, 0, len(stores))
	for _, st := range stores {
		c := census[st]
		parts = append(parts, fmt.Sprintf("%s %d rows (%d redacted, %d unscanned)", st, c.Total, c.Redacted, c.Skipped))
	}
	return strings.Join(parts, "; ")
}

// SetRedactionSeams records how the container wired the content-store
// decorators — the worst bypass reason any carries and the stores present —
// and where to read the live census. Without it the check reports SKIPPED.
func (h *DoctorHandlers) SetRedactionSeams(reason auditredact.Reason, stores []string, census func() map[string]auditredact.SeamCount) {
	h.redactionSeamsKnown = true
	h.redactionSeamReason = reason
	h.redactionSeamStores = stores
	h.redactionSeamCensus = census
}

package api

import (
	"fmt"
	"strings"
	"time"
)

// ProvenanceCloneStatus is one configured memory.provenance_clone as the
// daemon sees it now (GitHub #499; design 22, "Amendment 2026-10-21 (#482)",
// part 8g): whether the verifier accepts the clone, and the sweep's last
// fetch of the trusted branch into it.
type ProvenanceCloneStatus struct {
	ProjectID string
	// Usable is false when the verifier refuses the clone (it no longer
	// resolves, or it resolves into the agent-mounted workspace root).
	Usable bool
	// Fetched is false until the sweep has fetched once since start.
	Fetched      bool
	FetchOutcome string
	FetchError   string
	FetchedAt    time.Time
}

// SetProvenanceClones wires the memory_provenance_clone check; nil leaves it
// SKIPPED.
func (h *DoctorHandlers) SetProvenanceClones(fn func() []ProvenanceCloneStatus) {
	h.provenanceClones = fn
}

// SetProvenanceForgeNotes wires the forge lines of memory_provenance_clone
// (#508 F6).
func (h *DoctorHandlers) SetProvenanceForgeNotes(fn func() (forges, refusals []string)) {
	h.provenanceForgeNotes = fn
}

// forgeNotes appends the configured forges and the recent refusals to a
// message; a refusal turns an OK into a WARNING, since each names its fix.
func (h *DoctorHandlers) forgeNotes(status, msg string) (string, string) {
	if h.provenanceForgeNotes == nil {
		return status, msg
	}
	forges, refusals := h.provenanceForgeNotes()
	if len(forges) > 0 {
		msg += "; forges: " + strings.Join(forges, "; ")
	}
	if len(refusals) > 0 {
		msg += fmt.Sprintf("; %d scope(s) refused in the last 24 h: ", len(refusals)) + strings.Join(refusals, "; ")
		status = "WARNING"
	}
	return status, msg
}

// checkProvenanceClone reports every configured daemon-only provenance
// clone: whether it is usable and how its last fetch went. It states how
// many it examined; with none configured the check is OK and says the
// workspace clones are the trust source.
func (h *DoctorHandlers) checkProvenanceClone() DoctorCheck {
	const name = "memory_provenance_clone"
	if h.provenanceClones == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "provenance clone status not wired"}
	}
	clones := h.provenanceClones()
	if len(clones) == 0 {
		status, msg := h.forgeNotes("OK", "0 provenance clones yet; each repository gets an automatic clone on its first companion document ingest")
		return DoctorCheck{Name: name, Status: status, Message: msg}
	}
	var bad, ok []string
	for _, c := range clones {
		switch {
		case !c.Usable:
			bad = append(bad, c.ProjectID+": clone refused (missing, or inside the workspace root)")
		case !c.Fetched:
			ok = append(ok, c.ProjectID+": not fetched yet since start")
		case c.FetchOutcome != "ok":
			bad = append(bad, fmt.Sprintf("%s: last fetch %s at %s: %s", c.ProjectID, c.FetchOutcome,
				c.FetchedAt.UTC().Format(time.RFC3339), c.FetchError))
		default:
			ok = append(ok, fmt.Sprintf("%s: fetched %s", c.ProjectID, c.FetchedAt.UTC().Format(time.RFC3339)))
		}
	}
	scope := fmt.Sprintf("%d provenance clones examined", len(clones))
	if len(clones) == 1 {
		scope = "1 provenance clone examined"
	}
	msg := scope
	if len(ok) > 0 {
		msg += "; " + strings.Join(ok, "; ")
	}
	if len(bad) == 0 {
		status, m := h.forgeNotes("OK", msg)
		return DoctorCheck{Name: name, Status: status, Message: m}
	}
	_, m := h.forgeNotes("WARNING", msg+"; "+strings.Join(bad, "; ")+
		"; chunks checked against a stale or missing clone stay or become unverified")
	return DoctorCheck{Name: name, Status: "WARNING", Message: m}
}

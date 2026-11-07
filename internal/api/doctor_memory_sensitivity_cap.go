package api

import (
	"context"
	"fmt"
)

// SensitivityCapRole is one role whose memorySearch policy caps the
// sensitivity tier it may retrieve (#618 C4; firewall design, "Amendment
// 2026-11-06 (#618)"), with the counts of its project's chunks.
type SensitivityCapRole struct {
	ProjectID, Role, MaxSensitivity string
	Public, Internal, Confidential  int // set by an operator
	Restricted                      int // set by an operator
	DerivedPublic                   int // no tier, proven published
	Unlabelled                      int // no tier and not derived public: ranks as confidential
	Admitted                        int // what the role's whole filter admits
	Err                             error
}

// SetSensitivityCapRoles wires memory_sensitivity_cap: every capped role, with
// its counts. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetSensitivityCapRoles(fn func(ctx context.Context) ([]SensitivityCapRole, error)) {
	h.sensitivityCapRoles = fn
}

// checkMemorySensitivityCap states, for each role with a sensitivity cap, how
// many of its project's chunks carry each tier and how many the cap admits. A
// role whose cap admits nothing would retrieve nothing, so it warns. It never
// names a chunk.
func (h *DoctorHandlers) checkMemorySensitivityCap(ctx context.Context) DoctorCheck {
	const name = "memory_sensitivity_cap"
	if h.sensitivityCapRoles == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "memory not wired"}
	}
	roles, err := h.sensitivityCapRoles(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not list capped roles: " + err.Error()}
	}
	if len(roles) == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no role sets memorySearch.maxSensitivity"}
	}
	status, empty := "OK", 0
	items := make([]string, 0, len(roles))
	for _, r := range roles {
		who := fmt.Sprintf("%s/%s (max %s)", r.ProjectID, r.Role, r.MaxSensitivity)
		if r.Err != nil {
			status = "WARNING"
			items = append(items, who+": could not count: "+r.Err.Error())
			continue
		}
		if r.Admitted == 0 {
			status, empty = "WARNING", empty+1
		}
		items = append(items, fmt.Sprintf("%s: admits %d; by tier: public %d, derived public %d, internal %d, confidential %d, restricted %d, unlabelled %d (ranked confidential)",
			who, r.Admitted, r.Public, r.DerivedPublic, r.Internal, r.Confidential, r.Restricted, r.Unlabelled))
	}
	msg := fmt.Sprintf("%d role(s) capped; counts are live chunks in each role's repo scope", len(roles))
	if empty > 0 {
		msg += fmt.Sprintf("; %d role(s) admit no chunk and would retrieve nothing: label chunks public or check memory.public_source_prefixes", empty)
	}
	if status == "WARNING" && empty == 0 {
		msg += "; some counts could not be read"
	}
	return DoctorCheck{Name: name, Status: status, Message: msg, Items: items}
}

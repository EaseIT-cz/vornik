package api

// Doctor check: project_dependencies.
//
// Slice 1 item 4 of the project dependency provisioning design
// (https://docs.vornik.io
// §7).
//
// WHY IT EXISTS, in the design's own terms: "the reviewer could not run the
// tests" must be answerable BEFORE a customer asks, and an unmaterialised
// cache is otherwise indistinguishable from a project with no dependencies.
// Both present, from inside an agent, as an ordinary ImportError — which reads
// like the code's fault rather than the provisioner's.
//
// So the check reports three states and keeps them apart:
//
//	ERROR    the manifest cannot produce a mount at all — a lockfile the
//	         project does not have, or one this design cannot key. The
//	         project's agents will refuse to start rather than review code
//	         they cannot run.
//	WARNING  the manifest is sound and the key is not materialised yet. Not
//	         a defect: a fresh install, a changed lockfile, or an air-gapped
//	         deployment awaiting `vornikctl deps install --from`. It IS a statement
//	         that the reviewer case does not work right now.
//	OK       every declared key is present and complete.

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/projectdeps"
)

// ProjectDependencyStatus is one project's resolved manifest.
type ProjectDependencyStatus struct {
	ProjectID string
	Plans     []projectdeps.Plan
	// StaleImages lists, per installed key, the project's role images the
	// tree was NOT installed for (design §8.2): the daemon refuses to mount it
	// for those roles.
	StaleImages map[string][]string
}

// DependencyInventory supplies the resolved manifest of every project that
// declares one. It resolves through the SAME planner the mount path uses, so
// the check cannot answer "is this project's cache ready" for a key the mount
// never asks for.
type DependencyInventory func() []ProjectDependencyStatus

// SetDependencyInventory wires the dependency provisioning inventory. Optional:
// unset means the check reports SKIPPED rather than OK.
func (h *DoctorHandlers) SetDependencyInventory(inv DependencyInventory) {
	h.depsInventory = inv
}

func (h *DoctorHandlers) checkProjectDependencies() DoctorCheck {
	name := "project_dependencies"

	if h.depsInventory == nil {
		// NOT CHECKED, and it says so. A control that cannot distinguish
		// "examined and clean" from "never examined" reports the first
		// and means the second.
		return DoctorCheck{
			Name:    name,
			Status:  "SKIPPED",
			Message: "dependency provisioning is not wired into this daemon; no project manifest was examined (this is NOT a statement that project dependencies are provisioned)",
		}
	}

	inventory := h.depsInventory()
	if len(inventory) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "no project declares a dependency manifest; agent containers get no dependency mount, which is the documented default",
		}
	}

	var broken, pending, stale []string
	declared := 0

	for _, proj := range inventory {
		for _, p := range proj.Plans {
			declared++
			switch {
			case p.Problem != nil:
				broken = append(broken, fmt.Sprintf("%s/%s: %v", proj.ProjectID, p.Entry.Ecosystem, p.Problem))
			case !p.Materialised:
				pending = append(pending, fmt.Sprintf("%s/%s (key %s): vornikctl deps install %s", proj.ProjectID, p.Entry.Ecosystem, p.Key, proj.ProjectID))
			case len(proj.StaleImages[p.Key]) > 0:
				stale = append(stale, fmt.Sprintf("%s/%s (key %s) not installed for %s: vornikctl deps install %s",
					proj.ProjectID, p.Entry.Ecosystem, p.Key, strings.Join(proj.StaleImages[p.Key], ", "), proj.ProjectID))
			}
		}
	}
	sort.Strings(broken)
	sort.Strings(pending)
	sort.Strings(stale)

	switch {
	case len(broken) > 0:
		msg := fmt.Sprintf("%d of %d declared dependency sets cannot be materialised: %s",
			len(broken), declared, strings.Join(broken, "; "))
		if len(pending) > 0 {
			msg += fmt.Sprintf(" (a further %d are sound but not yet materialised)", len(pending))
		}
		return DoctorCheck{Name: name, Status: "ERROR", Message: msg}
	case len(pending) > 0 || len(stale) > 0:
		// The daemon never installs (design §8): the remedy is the operator's
		// `vornikctl deps install`, run on the agent host (with --from
		// <wheelhouse> when air-gapped). Until then those projects' agents
		// refuse to start rather than run code they cannot import.
		parts := append(append([]string{}, pending...), stale...)
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("%d of %d declared dependency sets are not installed for the images their roles run, so those agents refuse to start: %s. "+
				"Install on the agent host (add --from <wheelhouse> when air-gapped). This check compares image NAMES; after an agent-image rebuild under the same tag, `vornikctl deps status` compares image IDs",
				len(pending)+len(stale), declared, strings.Join(parts, "; ")),
		}
	default:
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("all %d declared dependency sets across %d projects are installed for their roles' images", declared, len(inventory)),
		}
	}
}

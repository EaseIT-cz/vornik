package executor

import (
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/registry"
)

// Staging follows what the role can read (media routing LLD §4.2a,
// 2026-09-25). extractTaskInputArtifacts skips raw staging of an extracted
// document on the assumption the agent reads it with the document_* tools. That
// assumption is checked here, per role, before staging: a workflow in which any
// step's role cannot open an extraction stages as if it declared
// require_input_artifacts (same cap). T-8f69 is the incident: a builtins-only
// reviewer made 17 refused document_get_outline calls and reviewed nothing.

const (
	docToolOutline = "mcp__vornik__document_get_outline"
	docToolRead    = "mcp__vornik__document_read_section"

	causeOutlineMissing     = "outline_missing"
	causeReadSectionMissing = "read_section_missing"
	causeBothMissing        = "both_missing"
	causeUnresolvedRole     = "unresolved_role"
)

// fileReaderTools are the ways a role can read a staged file. A role whose
// allowlist admits none of them cannot read ANY attached file; that gap is out
// of §4.2a's scope and is counted, not fixed.
var fileReaderTools = []string{"file_read", "read_many_files", "grep", "run_shell"}

// extractionAccess reports whether roleName can OPEN an extraction: its
// allowlist admits both document_get_outline (to find a section) and
// document_read_section (to page it). memory_search does not count: every role
// has it, and it returns passages, not the document.
//
// It uses agenttools.AllowlistAdmits, the predicate /mcp/call applies, so the
// two agree for every role that resolves (an empty allowlist is able). The one
// deliberate disagreement is a role that does NOT resolve: /mcp/call fails
// open there, and this fails CLOSED, because concluding "can read" for a role
// that cannot is exactly T-8f69, while the closed direction costs one staged
// file within the cap.
func extractionAccess(swarm *registry.Swarm, roleName string) (able bool, cause string) {
	role, err := findSwarmRole(swarm, roleName)
	if err != nil {
		return false, causeUnresolvedRole
	}
	allowed := role.Permissions.AllowedTools
	outline := agenttools.AllowlistAdmits(allowed, docToolOutline)
	read := agenttools.AllowlistAdmits(allowed, docToolRead)
	switch {
	case outline && read:
		return true, ""
	case !outline && !read:
		return false, causeBothMissing
	case !outline:
		return false, causeOutlineMissing
	default: // outline admitted, read_section not
		return false, causeReadSectionMissing
	}
}

// hasFileReader reports whether roleName can read a staged file at all.
func hasFileReader(swarm *registry.Swarm, roleName string) bool {
	role, err := findSwarmRole(swarm, roleName)
	if err != nil {
		return false
	}
	for _, tool := range fileReaderTools {
		if agenttools.AllowlistAdmits(role.Permissions.AllowedTools, tool) {
			return true
		}
	}
	return false
}

// inputStagingDecision is the task-start answer to "must extracted documents
// be staged raw?".
type inputStagingDecision struct {
	// force stages extracted documents within the cap: the workflow declared
	// require_input_artifacts, or a step's role cannot open extractions.
	force bool
	// unable maps each role that cannot open extractions to why.
	unable map[string]string
	// noReader lists roles with none of the four file readers.
	noReader []string
}

// decideInputStaging walks every step of the plan's workflow once.
func decideInputStaging(plan *executionPlan) inputStagingDecision {
	var d inputStagingDecision
	if plan == nil || plan.workflow == nil {
		return d
	}
	d.force = plan.workflow.RequireInputArtifacts
	seen := map[string]bool{}
	for _, role := range candidateRoles(plan) {
		if seen[role] {
			continue
		}
		seen[role] = true
		if able, cause := extractionAccess(plan.swarm, role); !able {
			if d.unable == nil {
				d.unable = map[string]string{}
			}
			d.unable[role] = cause
			d.force = true
		}
		if !hasFileReader(plan.swarm, role) {
			d.noReader = append(d.noReader, role)
		}
	}
	sort.Strings(d.noReader)
	return d
}

// candidateRoles lists every role that may run a step of the plan's workflow:
// each step's declared role, and, when the workflow has a plan step, every
// role in the swarm, because a plan step runs whichever roles its lead picks
// (plan_step.go) and none of them is declared in the workflow.
func candidateRoles(plan *executionPlan) []string {
	var out []string
	adaptive := false
	for _, step := range plan.workflow.Steps {
		if step.Type == "plan" {
			adaptive = true
		}
		if step.Role != "" {
			out = append(out, step.Role)
		}
	}
	if adaptive && plan.swarm != nil {
		for _, r := range plan.swarm.Roles {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// extractedInputBasenames returns the basenames of the task's input files that
// were extracted, by the same rule extractTaskInputArtifacts applies.
func extractedInputBasenames(payload []byte) []string {
	in, ok := parseTaskInputContext(payload)
	if !ok {
		return nil
	}
	marked := extractedBasenameSet(in.InputFiles, in.InputExtractions)
	out := make([]string, 0, len(marked))
	for base := range marked {
		out = append(out, base)
	}
	sort.Strings(out)
	return out
}

// observeInputStaging counts and logs what the unable-role rule did, once per
// task at staging time. Nothing is recorded when every role can open
// extractions: that is the clean path.
func (e *Executor) observeInputStaging(taskID string, payload []byte, staged []map[string]string, d inputStagingDecision) {
	if len(d.unable) == 0 && len(d.noReader) == 0 {
		return
	}
	stagedSet := make(map[string]bool, len(staged))
	for _, a := range staged {
		stagedSet[filepath.Base(a["name"])] = true
	}
	roles := make([]string, 0, len(d.unable))
	for r := range d.unable {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	if len(d.unable) > 0 {
		for _, base := range extractedInputBasenames(payload) {
			outcome := "over_cap"
			if stagedSet[base] {
				outcome = "staged"
			}
			for _, r := range roles {
				e.metrics.RecordInputStagingRoleUnable(outcome, d.unable[r])
			}
			e.logger.Warn().Str("task_id", taskID).Str("file", base).Str("outcome", outcome).
				Str("roles", strings.Join(roles, ",")).
				Msg("input staging: a role in this workflow cannot open extractions with the document tools")
		}
	}
	// The two counters are deliberately non-exclusive (§4.2a): this one is
	// about reading the STAGED file, the one above about opening the extraction.
	for range staged {
		for range d.noReader {
			e.metrics.RecordInputStagingNoFileReader()
		}
	}
}

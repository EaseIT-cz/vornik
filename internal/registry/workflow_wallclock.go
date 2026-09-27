package registry

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A STEP TIMEOUT THE EXECUTION CAN NEVER REACH (loader-validator design §14).
//
// maxWallClock is the EXECUTION's deadline: the executor wraps the whole run's
// context in it before any step is dispatched, so it caps every step inside.
// A step whose declared timeout is larger can never use the difference, and
// nothing used to say so. Incident 2026-09-23: the companion
// architectural-review step was raised 814s → 1800s under `maxWallClock: "20m"`
// and reviews kept dying at exactly 20m00s.

// WallClockCap is THE rule for what a maxWallClock value arms, shared by every
// reader: the executor that arms the deadline, this validator, and the control
// plane's step-timeout renderer and apply-time re-check (actionable-proposals
// design §12, review 53a1 F3 — three readers with three parses can disagree on
// exactly the edge a clamp depends on). d > 0 is the cap; d == 0 is no cap —
// unset, non-positive, or unparseable, the last also returned as err so the
// executor can WARN. The raw value is parsed as written, not trimmed: the
// executor never trimmed, and it is the one that decides what runs.
func WallClockCap(raw string) (d time.Duration, err error) {
	if raw == "" {
		return 0, nil
	}
	d, err = time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, err
	}
	return d, nil
}

// WallClockFinding is one step whose declared timeout exceeds the workflow's
// maxWallClock, with the parsed values the finding was computed from — so a
// caller comparing two workflows compares durations, not rendered text
// (self-healing genome design, 2026-09-24: "20m" and "1200s" are one cap).
type WallClockFinding struct {
	StepID  string
	Timeout time.Duration
	Cap     time.Duration
	Finding WorkflowMDFinding
}

// WallClockFindings returns every step of wf whose declared timeout is
// strictly greater than its maxWallClock, sorted by step id. The validator's
// step_timeout_exceeds_wall_clock warning is built from exactly this list.
//
// WARNING, never an error: a cap tighter than one step is sometimes deliberate
// (fail fast rather than let one slow step own the run), and an error would
// reject a file that runs. DECLARED timeout only: the dispatch-time
// tool-budget scaling depends on the task and can shrink as well as grow.
// NOT the model-fallback budget: it exceeds the cap on a quarter of the
// shipped corpus, and a warning that fires everywhere is read nowhere.
func WallClockFindings(wf *Workflow) []WallClockFinding {
	if wf == nil || len(wf.Steps) == 0 {
		return nil
	}
	capRaw := wf.MaxWallClock
	wallClock, _ := WallClockCap(capRaw)
	if wallClock == 0 {
		// Unset means no cap; unparseable is the executor's own WARN and
		// treated as unset there — nothing to exceed either way.
		return nil
	}
	ids := make([]string, 0, len(wf.Steps))
	for id := range wf.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []WallClockFinding
	for _, id := range ids {
		raw := strings.TrimSpace(wf.Steps[id].Timeout)
		timeout, err := time.ParseDuration(raw)
		if raw == "" || err != nil || timeout <= wallClock {
			continue
		}
		out = append(out, WallClockFinding{
			StepID: id, Timeout: timeout, Cap: wallClock,
			Finding: WorkflowMDFinding{
				Severity: SeverityWarning,
				Code:     "step_timeout_exceeds_wall_clock",
				Field:    fmt.Sprintf("steps.%s.timeout", id),
				Message: fmt.Sprintf(
					"step %q declares timeout %s, but the workflow's maxWallClock is %s. maxWallClock is the "+
						"execution's deadline and caps every step, so the run is cancelled at %s and this step can "+
						"never use the rest of its timeout — raising it has no effect.",
					id, raw, capRaw, capRaw),
				Hint: "Raise maxWallClock to cover the step (and the steps before it), or lower the step's timeout to what the cap allows.",
			},
		})
	}
	return out
}

// appendWallClockFindings warns once per step whose declared timeout is
// strictly greater than the workflow's maxWallClock (see WallClockFindings).
func appendWallClockFindings(report *WorkflowMDValidationReport, wf *Workflow) {
	for _, f := range WallClockFindings(wf) {
		report.Findings = append(report.Findings, f.Finding)
	}
}

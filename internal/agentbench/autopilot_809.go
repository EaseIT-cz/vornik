package agentbench

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): the harness autopilot's decision. A blackbox arm learns from the
// warm-up pass: after every few warm-up tasks, a workflow whose failure rate
// over the runs since its last trigger is at or above the threshold, with at
// least min_samples runs, is due a trigger. The outcome read is the
// pipeline's own (TaskRun.Succeeded); the hidden acceptance verdict never
// reaches the learning system.

import "time"

// AutopilotRun is one warm-up task run, in the order it ran.
type AutopilotRun struct {
	Workflow     string
	Succeeded    bool
	ExecutionIDs []string
}

// AutopilotState is what the autopilot has done so far in one arm, kept in
// autopilot.json beside the journals.
type AutopilotState struct {
	Workflows map[string]*AutopilotWorkflow `json:"workflows"`
	Events    []AutopilotEvent              `json:"events"`
}

// AutopilotWorkflow is one workflow's autopilot history.
type AutopilotWorkflow struct {
	RunsAtLastTrigger int    `json:"runsAtLastTrigger"`
	LastTriggerID     string `json:"lastTriggerId,omitempty"`
}

// AutopilotEvent is one step the autopilot took, journaled as it happens.
type AutopilotEvent struct {
	At          time.Time `json:"at"`
	Step        string    `json:"step"` // trigger, generate, trial, promote, stop
	Workflow    string    `json:"workflow,omitempty"`
	TriggerID   string    `json:"triggerId,omitempty"`
	CandidateID string    `json:"candidateId,omitempty"`
	GenomeHash  string    `json:"genomeHash,omitempty"`
	Verdict     string    `json:"verdict,omitempty"`
	TaskRuns    int       `json:"taskRuns"`
	Detail      string    `json:"detail,omitempty"`
}

// AutopilotDue is a workflow due a trigger.
type AutopilotDue struct {
	Workflow             string
	Runs                 int     // runs since the last trigger
	FailureRate          float64 // over those runs
	EvidenceExecutionIDs []string
	RunsTotal            int // the workflow's warm-up runs so far
}

// DueTriggers decides which workflows are due a trigger now.
func DueTriggers(runs []AutopilotRun, st AutopilotState, minSamples int, threshold float64) []AutopilotDue {
	byWorkflow := map[string][]AutopilotRun{}
	var order []string
	for _, r := range runs {
		if _, seen := byWorkflow[r.Workflow]; !seen {
			order = append(order, r.Workflow)
		}
		byWorkflow[r.Workflow] = append(byWorkflow[r.Workflow], r)
	}
	var due []AutopilotDue
	for _, wf := range order {
		all := byWorkflow[wf]
		since := all
		if h := st.Workflows[wf]; h != nil {
			if h.RunsAtLastTrigger >= len(all) {
				continue // no run since the last trigger (or the journals were cut)
			}
			since = all[h.RunsAtLastTrigger:]
		}
		if len(since) < minSamples {
			continue
		}
		var evidence []string
		failed := 0
		for _, r := range since {
			if !r.Succeeded {
				failed++
				evidence = append(evidence, r.ExecutionIDs...)
			}
		}
		rate := float64(failed) / float64(len(since))
		if failed == 0 || rate < threshold {
			continue
		}
		due = append(due, AutopilotDue{Workflow: wf, Runs: len(since), FailureRate: rate, EvidenceExecutionIDs: evidence, RunsTotal: len(all)})
	}
	return due
}

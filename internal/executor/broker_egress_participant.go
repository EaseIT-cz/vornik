package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"vornik.io/vornik/internal/pipeline"
	"vornik.io/vornik/internal/registry"
)

// brokerEgressParticipant judges a broker workflow's answer at the step that
// writes it (broker design §17). The answer used to be judged only when
// result() was called, after the task had completed, so a three-step run
// whose summary was 603 characters against a 600 bound (task
// task_20261003003901_067399b47e7fc6f2) was lost whole. A refusal here
// carries the "schema violation:" prefix, so the existing corrective shape
// retry gives the step one turn to fix it. result() still judges the stored
// answer with the same validator, and its content guards stay there.
func (e *Executor) brokerEgressParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	wf := e.brokerWorkflowOf(in)
	if wf == nil || !slices.Contains(wf.AnswerSteps(), in.StepID) {
		return pipeline.Verdict{}
	}
	name := wf.Broker.Egress.Output
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return pipeline.Verdict{}
	}
	shown := "artifacts/out/" + name
	path := filepath.Join(in.WorkspaceDir, "artifacts", "out", name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.ModTime().Before(in.StepStart.Add(-outputContractMtimeSlack)) {
		return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
			"schema violation: the workflow's answer %s was not written during step %q. Write it before finishing.", shown, in.StepID)}
	}
	body, err := readCapped(path, wf.Broker.Egress.EffectiveMaxBytes()+1)
	if err != nil {
		return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
			"schema violation: the workflow's answer %s could not be read: write it again before finishing.", shown)}
	}
	v := registry.ValidateBrokerEgress(wf, body)
	switch v.Class {
	case "":
		return pipeline.Verdict{}
	case registry.EgressClassOversize:
		return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
			"schema violation: the workflow's answer %s is %d bytes, above the %d-byte limit; shorten it and write it again.", shown, v.Bytes, v.Limit)}
	}
	return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
		"schema violation: the workflow's answer %s does not validate against the approved schema: %s. Fix exactly these and write it again; a length bound counts characters.",
		shown, strings.Join(v.Messages, "; "))}
}

// brokerWorkflowOf is the execution's workflow when it is a broker workflow.
func (e *Executor) brokerWorkflowOf(in *StepOutcome) *registry.Workflow {
	if e.workflows == nil || in.Execution == nil || in.Execution.WorkflowID == "" {
		return nil
	}
	wf := e.workflows.GetWorkflow(in.Execution.WorkflowID)
	if wf == nil || wf.Broker == nil {
		return nil
	}
	return wf
}

func readCapped(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	return io.ReadAll(io.LimitReader(f, int64(limit)))
}

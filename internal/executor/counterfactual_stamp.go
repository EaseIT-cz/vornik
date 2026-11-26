package executor

import (
	"context"

	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/persistence"
)

// CounterfactualStamper writes a replay's provenance onto the execution row
// executionID (GitHub #912). The EE provider set builds it with
// blackbox.NewCounterfactualStamper, a Postgres UPDATE of those columns by
// the row's ID; SQLite has no such columns and wires nothing.
type CounterfactualStamper func(ctx context.Context, executionID, originalTaskID, variable, label string) error

// WithCounterfactualStamper sets the stamper the executor calls right
// after it creates an execution row for a replay task.
func WithCounterfactualStamper(f CounterfactualStamper) Option {
	return func(e *Executor) { e.counterfactualStamper = f }
}

// stampCounterfactual stamps execution with its task's replay provenance,
// when the task carries one (context.counterfactual.provenance). The
// engine used to stamp at task creation, before this row existed, and
// missed on every replay (#912). A failure is a warning: the provenance
// link is lost, the execution runs.
func (e *Executor) stampCounterfactual(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	if e.counterfactualStamper == nil || task == nil || execution == nil {
		return
	}
	p := counterfactual.ExtractPayload(task.Payload).Provenance
	if !p.Known() {
		return
	}
	if err := e.counterfactualStamper(ctx, execution.ID, p.OriginalTaskID, p.Variable, p.Label); err != nil {
		e.logger.Warn().Err(err).Str("task_id", task.ID).Str("execution_id", execution.ID).
			Str("counterfactual_of_task_id", p.OriginalTaskID).
			Msg("counterfactual: provenance not stamped on the replay's execution; the link to the original is lost, the execution runs")
	}
}

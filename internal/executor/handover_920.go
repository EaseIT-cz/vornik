package executor

// Agent-administered design part 24 §24.3 B2-B3 (GitHub #920): a COMPLETED
// agent job whose workflow declares broker.hand_over starts the target
// workflow of the same namespace. The executor decides only whether to try
// and reports the outcome; the daemon side (FireHandover) runs every check
// a scheduled run does.

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// HandOverFirer starts a hand-over's target task for a COMPLETED source
// task. A refusal is a *HandOverRefusal; any other error is a failure.
type HandOverFirer interface {
	FireHandover(ctx context.Context, source *persistence.Task, execution *persistence.Execution, wf *registry.Workflow) (string, error)
}

// HandOverRefusal is a rule saying no (§24.3 B3): Reason is a fixed code
// (depth, missing_document, inputs, budget, setup, not_delegable), never a
// value.
type HandOverRefusal struct{ Reason, Detail string }

func (r *HandOverRefusal) Error() string { return "hand-over refused (" + r.Reason + "): " + r.Detail }

// HandOvers counts hand-over attempts by namespace and outcome: started,
// refused, failed.
var HandOvers = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "agent_handovers_total",
	Help: "Agent job hand-overs to another workflow of the namespace, by outcome: started, refused (a rule said no), failed (the call errored) (GitHub #920).",
}, []string{"namespace", "outcome"})

// WithHandOverFirer wires the daemon side of hand_over.
func WithHandOverFirer(f HandOverFirer) Option {
	return func(e *Executor) { e.handOverFirer = f }
}

// handOver runs a COMPLETED task's declared hand-over. It never changes the
// source task.
func (e *Executor) handOver(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	if e.workflows == nil || execution == nil || execution.WorkflowID == "" || task.Status != persistence.TaskStatusCompleted {
		return
	}
	wf := e.workflows.GetWorkflow(execution.WorkflowID)
	if wf == nil || wf.Broker == nil || wf.Broker.HandOver == nil {
		return
	}
	ns, _ := agentns.FromID(wf.ID)
	target := wf.Broker.HandOver.Workflow
	log := e.logger.With().Str("task_id", task.ID).Str("workflow_id", wf.ID).Str("hand_over_to", target).Logger()
	if e.handOverFirer == nil {
		HandOvers.WithLabelValues(ns, "failed").Inc()
		log.Warn().Msg("hand-over: not wired on this daemon; the target was not started")
		return
	}
	id, err := e.handOverFirer.FireHandover(ctx, task, execution, wf)
	var refused *HandOverRefusal
	switch {
	case err == nil:
		HandOvers.WithLabelValues(ns, "started").Inc()
		log.Info().Str("target_task_id", id).Msg("hand-over: started the target workflow")
		e.emitLive(ctx, execution.ID, livepubsub.KindHandOverStarted, map[string]any{"target_workflow": target, "target_task_id": id})
	case errors.As(err, &refused):
		HandOvers.WithLabelValues(ns, "refused").Inc()
		log.Warn().Str("reason", refused.Reason).Str("detail", refused.Detail).Msg("hand-over: refused; the target was not started")
		e.emitLive(ctx, execution.ID, livepubsub.KindHandOverRefused, map[string]any{"target_workflow": target, "reason": refused.Reason})
	default:
		HandOvers.WithLabelValues(ns, "failed").Inc()
		log.Warn().Err(err).Msg("hand-over: failed; the target was not started")
		e.emitLive(ctx, execution.ID, livepubsub.KindHandOverRefused, map[string]any{"target_workflow": target, "reason": "failed"})
	}
}

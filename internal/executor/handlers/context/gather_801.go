// Package context holds the context.gather system step (GitHub #801).
package context

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/executor"
)

// gatherBudget is the chain's total (systemResultMaxBytes): a gathered
// message that does not fit whole would be cut by the chain.
const gatherBudget = 256 << 10

// GatherHandler re-emits named earlier steps' messages, in order, so the
// next agent receives them (github-app-channel design, "A review good by
// default"): an agent after another agent otherwise sees only that one.
type GatherHandler struct{}

// Name implements executor.SystemHandler.
func (GatherHandler) Name() string { return "context.gather" }

// Execute renders each named step's message under a heading. It fails,
// naming the step, when one is missing or the whole does not fit: a list
// dropped silently would read as no list.
func (GatherHandler) Execute(_ stdctx.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	if in.Step == nil || len(in.Step.Gather) == 0 {
		return executor.SystemStepResult{}, fmt.Errorf("context.gather: the step names no earlier steps (gather)")
	}
	var b strings.Builder
	for _, id := range in.Step.Gather {
		raw, ok := in.StepResults[id]
		if !ok {
			return executor.SystemStepResult{}, fmt.Errorf("context.gather: step %q has no result", id)
		}
		var env struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &env)
		fmt.Fprintf(&b, "## From step %s\n\n%s\n\n", id, env.Message)
		if b.Len() > gatherBudget {
			return executor.SystemStepResult{}, fmt.Errorf("context.gather: the sections up to %q are %d bytes, over the %d the next agent can receive whole", id, b.Len(), gatherBudget)
		}
	}
	out, _ := json.Marshal(map[string]any{"message": b.String()})
	return executor.SystemStepResult{Result: out}, nil
}

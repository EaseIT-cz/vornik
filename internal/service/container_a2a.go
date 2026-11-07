package service

// A2A wiring helpers — adapter shims between the existing
// service-container dependencies and the narrow interfaces
// internal/conversation/a2a expects. Lives in its own file so the
// boot path stays focused on lifecycle, not the protocol-specific
// translation glue.

import (
	"context"
	"encoding/json"

	a2aclient "vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/a2a/consult"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

// buildConsultProvider materialises the A2A consult-tool provider
// (mcp__consult__<peer>) from a2a.peers config, or nil when no peers are
// configured. Called at both wiring sites — the container-agent
// ComposedMCPExecutor and the dispatcher's mcpManager — so agents on either
// surface can consult domain experts. Each site gets its own instance; a task
// never spans both paths, so the per-task consult counter needn't be shared.
func (c *Container) buildConsultProvider() *consult.Provider {
	if len(c.Config.A2A.Peers) == 0 {
		return nil
	}
	// #640: every inbound A2A task on a published expert is created by a
	// consult, so a consult made from it must carry its hop count plus one.
	var hops consult.TaskHopLookup
	if c.repos != nil && c.repos.Tasks != nil {
		hops = taskHopLookup{tasks: c.repos.Tasks}
	}
	return consult.New(c.Config.A2A.Peers, c.Config.A2A.Consult, a2aclient.New(), hops)
}

// a2aTaskCreatorAdapter wraps *taskcreate.Creator so it satisfies
// the a2a.TaskCreator interface without leaking the wider Params
// surface (idempotency, raw context, etc.) into the protocol
// package. Translation is a thin field copy.
type a2aTaskCreatorAdapter struct {
	inner *taskcreate.Creator
}

// Create translates the A2A submit payload into a taskcreate.Params
// and delegates. The CreationSource flows through verbatim so the
// audit trail records "A2A" rather than "USER" for tasks the
// protocol surface spawned.
func (a a2aTaskCreatorAdapter) Create(ctx context.Context, p a2a.TaskCreateParams) (*persistence.Task, error) {
	return a.inner.Create(ctx, taskcreate.Params{
		ProjectID:      p.ProjectID,
		WorkflowID:     p.WorkflowID,
		TaskType:       p.TaskType,
		Prompt:         p.Prompt,
		Priority:       p.Priority,
		CreationSource: p.CreationSource,
		ExtraContext:   p.ExtraContext,
		// The adapter serves only the A2A surface: the caller wrote the goal.
		GoalOrigin: persistence.TaskGoalOriginA2A,
		// The key that submitted it, so its budget cap and daily quota see
		// the task (#617; federation design, amendment 2026-11-06).
		CreatedByAPIKeyID: api.APIKeyIDFromContext(ctx),
	})
}

// taskHopLookup reads the consult hop count an inbound A2A consult stored on
// the task it created (#640; federation design §6): the payload's
// context.a2a.metadata, or the nearest ancestor's, so a consult made from a
// subtask of that task counts too. Anything unreadable is hop 0.
type taskHopLookup struct {
	tasks interface {
		Get(ctx context.Context, id string) (*persistence.Task, error)
	}
}

// maxHopAncestors bounds the parent walk.
const maxHopAncestors = 10

func (l taskHopLookup) InboundConsultHops(ctx context.Context, taskID string) int {
	if l.tasks == nil {
		return 0
	}
	id := taskID
	for i := 0; i < maxHopAncestors && id != ""; i++ {
		tk, err := l.tasks.Get(ctx, id)
		if err != nil || tk == nil {
			return 0
		}
		var p struct {
			Context struct {
				A2A *struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"a2a"`
			} `json:"context"`
		}
		if json.Unmarshal(tk.Payload, &p) == nil && p.Context.A2A != nil {
			if f, ok := p.Context.A2A.Metadata[config.ConsultHopHeader].(float64); ok && f >= 0 && f <= 1000 && f == float64(int(f)) {
				return int(f)
			}
			return 0
		}
		if tk.ParentTaskID == nil {
			return 0
		}
		id = *tk.ParentTaskID
	}
	return 0
}

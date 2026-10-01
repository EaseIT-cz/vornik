package persistence

import "context"

// Companion push outbox — https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md §7a.
//
// The state to push is already stored: a companion task's status, and a
// broker action's status. A pushed_state column beside each (on
// a2a_push_configs and on broker_actions) records the name last delivered,
// and the pusher loop sends the difference. Both kinds are for
// companion-created tasks only.

// TaskPushDue is a terminal companion task whose status was not yet pushed.
type TaskPushDue struct {
	TaskID      string
	ProjectID   string
	State       string // the task status to push
	URL         string
	Token       string
	PushedState *string // the value to compare-and-set against
}

// ActionPushDue is a broker action whose front state (§6 names) was not yet
// pushed. URL is empty when the action's task has no push config: the row
// is then marked without sending.
type ActionPushDue struct {
	TaskID      string
	ProjectID   string
	ActionID    string
	Action      string
	State       string // the §6 front state to push
	URL         string
	Token       string
	PushedState *string
}

// CompanionPushOutbox reads what is due and records what was delivered.
type CompanionPushOutbox interface {
	// DueTaskPushes lists terminal (COMPLETED, FAILED, CANCELLED)
	// companion tasks with a push config whose pushed_state differs from
	// their status, oldest first.
	DueTaskPushes(ctx context.Context, limit int) ([]TaskPushDue, error)
	// MarkTaskPushed sets the config's pushed_state to state if it still
	// equals prev (nil = NULL). True when this call set it.
	MarkTaskPushed(ctx context.Context, taskID string, prev *string, state string) (bool, error)
	// DueActionPushes lists actions of companion tasks whose front state is
	// pushable (pending_approval, approved, rejected, expired, executed,
	// failed, unknown) and differs from pushed_state, oldest first.
	DueActionPushes(ctx context.Context, limit int) ([]ActionPushDue, error)
	// MarkActionPushed is MarkTaskPushed for an action.
	MarkActionPushed(ctx context.Context, actionID string, prev *string, state string) (bool, error)
}

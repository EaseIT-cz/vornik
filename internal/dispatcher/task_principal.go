package dispatcher

import "context"

// The created task's principal for a chat turn (GitHub #499, operator
// decision A19; design 22, "Amendment 2026-10-21 (#482)", part 8a). The
// channel decides it from its own identity check (Request.TaskPrincipal) and
// create_task stamps it on the task's created_by_actor. Memory verification
// trusts a USER task only with an attributable operator, so a turn the channel
// did not attribute (an unlinked speaker, a group conversation, a synthesised
// follow-up) creates an unattributed task.

type taskPrincipalCtxKey struct{}

// withTaskPrincipal carries the channel's principal for the turn. Empty is
// not recorded.
func withTaskPrincipal(ctx context.Context, principal string) context.Context {
	if principal == "" {
		return ctx
	}
	return context.WithValue(ctx, taskPrincipalCtxKey{}, principal)
}

// taskPrincipalFromContext is the turn's principal, or "".
func taskPrincipalFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(taskPrincipalCtxKey{}).(string)
	return v
}

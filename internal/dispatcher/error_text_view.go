package dispatcher

import (
	"context"

	"vornik.io/vornik/internal/chatview"
	"vornik.io/vornik/internal/report"
)

// The chat tools' view of a task's stored error text (process-spawn-law
// design, "Amendment 2026-10-21 (#480)", J1): get_task_status and
// wait_for_task hand the model last_error and error_message raw only when the
// channel established that this turn's audience is one admin-linked principal
// (Request.OperatorView); every other turn reads report.ScrubLines of the
// text, the #440 primitive. The zero value is the scrubbed view, so a turn no
// channel classified (a synthesised follow-up, a new channel, a test) never
// reads the raw text.

type operatorViewCtxKey struct{}

// withOperatorView marks the turn's context with the channel's decision. Only
// true is recorded; absence reads as false.
func withOperatorView(ctx context.Context, operatorView bool) context.Context {
	if !operatorView {
		return ctx
	}
	return context.WithValue(ctx, operatorViewCtxKey{}, true)
}

// operatorViewFromContext reports whether the turn is an operator-view turn.
func operatorViewFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(operatorViewCtxKey{}).(bool)
	return v
}

// WithErrorTextScrubber gives the task tools the line scrubber for a stored
// error text (the service passes Container.logScrubber(), the one the UI and
// the explainer use). Without one, a turn that is not an operator-view turn
// reads every line withheld: the tool still answers, and serves nothing raw.
func WithErrorTextScrubber(s report.LineScrubber) AgentOption {
	return func(a *Agent) { a.errorScrub = s }
}

// errorTextView returns the turn's view of a stored error text: the one rule
// the non-tool chat readers share (chatview.View, #497 L1).
func (te *ToolExecutor) errorTextView(ctx context.Context) func(string) string {
	return chatview.View(operatorViewFromContext(ctx), te.errorScrub)
}

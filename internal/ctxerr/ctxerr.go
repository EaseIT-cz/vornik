// Package ctxerr decides why a bounded call ended: the caller cancelled it,
// its deadline expired, or the thing it called failed on its own.
//
// Six call sites used to make that decision each in their own way (the chat
// HTTP client, the chat stream, the CLI provider classifier, the warm-pool
// wait, the priority queue and the podman wait), and the differences were the
// defects: a caller cancel counted as a model timeout, a deadline counted as a
// plain error. Outcome-quality LLD §S2 (2026-10-12, GitHub #142) centralises
// the rule here.
//
// Classify only classifies. It never wraps, logs or counts; callers keep their
// own error text and metrics and take the label from here.
package ctxerr

import (
	"context"
	"errors"
)

// Cause is why a call ended. The string values are metric-label spellings
// already in use: "timeout" and "error" on the chat request counter,
// "cancelled" on the step-outcome taxonomy.
type Cause string

const (
	// None — the call returned no error and its context is still live.
	None Cause = ""
	// Cancelled — the caller went away (operator cancel, shutdown, lost
	// lease). Not a failure of the thing that was called.
	Cancelled Cause = "cancelled"
	// Timeout — a deadline expired. The thing that was called was too slow.
	Timeout Cause = "timeout"
	// Upstream — any other error: the thing that was called failed.
	Upstream Cause = "error"
)

// Classify reports why a call that ran under ctx and returned err ended.
//
// The context is read first: a finished context explains whatever error the
// call returned, including errors that do not wrap the context's (os/exec
// reports "signal: killed" for a child it killed because ctx ended). Then err
// is inspected with errors.Is. A cancel wins over a deadline in both places,
// the rule chat.IsUpstreamInfraError already applies: a caller that walked
// away is not evidence that the callee was slow.
//
// ctx may be nil.
func Classify(ctx context.Context, err error) Cause {
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			if errors.Is(cerr, context.Canceled) {
				return Cancelled
			}
			return Timeout
		}
	}
	switch {
	case err == nil:
		return None
	case errors.Is(err, context.Canceled):
		return Cancelled
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	default:
		return Upstream
	}
}

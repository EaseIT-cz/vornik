package executor

import (
	"errors"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// resolvedRetry is a step's retry ladder settings with every default already
// applied, so the loop reads one struct instead of branching on absent config.
type resolvedRetry struct {
	// MaxAttempts is the loop bound. Always positive.
	MaxAttempts int
	// BaseDelay is the first sleep. Always positive — a zero here would turn
	// the ladder into a hot loop.
	BaseDelay time.Duration
	// On is the set of step error classes this step retries IN ADDITION to
	// whatever isInfraFailure already recognises. Never subtractive.
	On map[string]bool
}

// resolveStepRetry applies a step's `retry:` block over the built-in defaults.
//
// Every unset or nonsensical field falls back to the constant, so a step with
// no block — or a block with a typo'd duration — behaves exactly as it did
// before step retry was configurable. That is the design's G3, and it is what
// makes this change safe to ship to ten workflows at once.
//
// Nonsense is clamped rather than honoured: a zero or negative max_attempts
// would disable retry entirely and an unparseable delay would resolve to zero,
// and neither is a thing an operator can have meant. Load-time validation
// rejects them too; this is the second line.
func resolveStepRetry(step registry.WorkflowStep) resolvedRetry {
	out := resolvedRetry{
		MaxAttempts: infraRetryMaxAttempts,
		BaseDelay:   infraRetryBaseDelay,
	}
	if step.Retry.MaxAttempts > 0 {
		out.MaxAttempts = step.Retry.MaxAttempts
	}
	if d, err := time.ParseDuration(step.Retry.InitialDelay); err == nil && d > 0 {
		out.BaseDelay = d
	}
	if len(step.Retry.On) > 0 {
		out.On = make(map[string]bool, len(step.Retry.On))
		for _, c := range step.Retry.On {
			out.On[c] = true
		}
	}
	return out
}

// stepClassError carries the error_class the step-outcome recorder wrote for a
// failed agent step, so the retry ladder decides on the SAME classification the
// operator sees. Error() is the wrapped error's text, byte for byte, so every
// message-based consumer is unaffected. It deliberately does NOT implement
// FailureClass(): that interface is the TASK failure vocabulary, read by
// ClassifyExecutionFailure, and a step class there would be a category error.
//
// Incident T-0d3c (2026-09-28): the row said verify_claims_failed while the
// ladder re-classified with the refiner alone, saw "unclassified", and re-ran a
// schema violation five times. Design: 2026-08-27-step-retry-configuration-
// design.md §9 D9.1.
type stepClassError struct {
	class string
	err   error
}

func (e *stepClassError) Error() string { return e.err.Error() }
func (e *stepClassError) Unwrap() error { return e.err }

// withStepClass attaches the recorded class to err. nil and an empty class
// pass through unchanged.
func withStepClass(err error, class string) error {
	if err == nil || class == "" {
		return err
	}
	return &stepClassError{class: class, err: err}
}

// recordedStepClass returns the class the outcome recorder attached, if any.
func recordedStepClass(err error) (string, bool) {
	var sce *stepClassError
	if errors.As(err, &sce) {
		return sce.class, true
	}
	return "", false
}

// shouldRetry reports whether this step's ladder should re-run after err.
//
// The class is the one the step-outcome recorder wrote (stepClassError), so
// what an operator sees in `error_class` is exactly what they write in `on:`.
// Only an error that never passed through the recorder falls back to the
// refiner.
//
// A recorded class outside stepoutcome's infra-retry allowlist is declined
// BEFORE isInfraFailure: that predicate matches text anywhere in the error,
// including the container-log tail, and a schema violation whose tail mentions
// a gateway 503 is still a schema violation (design §9 D9.2). Otherwise the
// built-in predicate is sufficient on its own and `on:` can only add to it.
func (r resolvedRetry) shouldRetry(err error) bool {
	class, recorded := recordedStepClass(err)
	if recorded && !stepoutcome.IsInfraRetryableClass(class) {
		return false
	}
	if isInfraFailure(err) {
		return true
	}
	if len(r.On) == 0 {
		return false
	}
	if !recorded {
		_, class = refineAgentFailureOutcomeErr(err)
	}
	return r.On[class]
}

// retryAttemptLimitReached is the single attempt-budget check used by the
// executor loop. Keeping it on the resolved config prevents the historical
// hard-coded default from silently truncating an explicit max_attempts value.
func retryAttemptLimitReached(attempt int, r resolvedRetry) bool {
	return attempt >= r.MaxAttempts
}

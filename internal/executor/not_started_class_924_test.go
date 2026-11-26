package executor

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// GitHub #924 (bench arm C, 2026-11-25 12:12): two analyst steps failed
// writing task.json ("disk quota exceeded") before their container started.
// The ledger called them "unclassified", and the bench's provenance check
// could not tell them from a step that ran without an image identity. A
// step whose container never started is classified container_start_failed
// from the type, the marker the bench reads.
func TestNotStartedStepIsClassifiedFromItsType_924(t *testing.T) {
	for name, err := range map[string]error{
		"input write": &containerNotStartedError{err: fmt.Errorf("failed to write task input: %w", errors.New("disk quota exceeded"))},
		"dependency":  &containerNotStartedError{err: errors.New("project dependencies: mount refused")},
		"wrapped":     fmt.Errorf("step analyze: %w", &containerNotStartedError{err: errors.New("failed to write task input: x")}),
	} {
		if _, c := refineAgentFailureOutcomeErr(err); c != stepoutcome.ClassContainerStartFailed {
			t.Errorf("%s: class %q, want %q", name, c, stepoutcome.ClassContainerStartFailed)
		}
	}
	// A container that ran keeps its own class.
	if _, c := refineAgentFailureOutcomeErr(errors.New("agent reported FAILED status: degenerate loop detected")); c != stepoutcome.ClassDegenerateLoop {
		t.Errorf("a step that ran was reclassified: %q", c)
	}
}

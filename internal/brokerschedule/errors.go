// Package brokerschedule runs agent broker workflows on their approved
// schedules (agent-administered Vornik design §17).
package brokerschedule

import "errors"

// The kinds of refused fire the scheduler counts separately (design §17.3).
var (
	// ErrInputs: the approved inputs do not satisfy the live input schema.
	ErrInputs = errors.New("the schedule's inputs no longer match the workflow's input schema")
	// ErrBudget: a budget refused the run.
	ErrBudget = errors.New("a budget refused the scheduled run")
)

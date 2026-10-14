package taskcancel

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// audit 2026-10-12: GitHub #142 §S3 #17 (outcome-quality LLD §S5 #17). A cancel
// the system makes on its own initiative records why on the row.

func TestCancelWith_PassesTheReasonToTheWrite(t *testing.T) {
	var got persistence.TransitionOpts
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			got = opts
			return true, nil
		},
	}
	reason, class := "approval timed out", persistence.TaskFailureClassCancelled
	out, err := CancelWith(context.Background(), repo, nil, "t1",
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TransitionOpts{LastError: &reason, LastErrorClass: &class})
	if err != nil || !out.Transitioned {
		t.Fatalf("CancelWith = %+v, %v", out, err)
	}
	if got.LastError == nil || *got.LastError != reason || got.LastErrorClass == nil || *got.LastErrorClass != class {
		t.Errorf("the write's options lost the reason: %+v", got)
	}
}

// The narrow TransitionToCancelled path takes no options; silently dropping
// them would lose the reason the caller meant to record.
func TestCancelWith_OptionsWithoutAFromSetAreRefused(t *testing.T) {
	called := false
	repo := &mocks.MockTaskRepository{
		TransitionToCancelledFunc: func(context.Context, string) (bool, error) {
			called = true
			return true, nil
		},
	}
	reason := "x"
	_, err := CancelWith(context.Background(), repo, nil, "t1", nil, persistence.TransitionOpts{LastError: &reason})
	if !errors.Is(err, ErrOptsNeedFromSet) {
		t.Fatalf("err = %v, want ErrOptsNeedFromSet", err)
	}
	if called {
		t.Error("the write ran and dropped the options")
	}
}

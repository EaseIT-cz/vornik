package service

// The self-restart's exit path (#758 W1): a requested restart leaves Run as
// the sentinel and the process with status 75, which Restart=on-failure
// restarts; a failure stays 1 and a clean stop 0.

import (
	"errors"
	"fmt"
	"testing"
)

func TestRestart758_ExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{ErrRestartRequested, 75},
		{fmt.Errorf("wrapped: %w", ErrRestartRequested), 75},
		{errors.New("boom"), 1},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestRestart758_RunResult(t *testing.T) {
	if !errors.Is(runResult(true, nil), ErrRestartRequested) {
		t.Error("a requested restart with a clean stop must return the sentinel")
	}
	boom := errors.New("boom")
	if got := runResult(true, boom); !errors.Is(got, ErrRestartRequested) || !errors.Is(got, boom) || ExitCode(got) != 75 {
		t.Errorf("a requested restart whose stop failed must still exit 75 and carry the failure: %v", got)
	}
	if runResult(false, boom) != boom {
		t.Error("a failed stop without a request reports the failure")
	}
	if runResult(false, nil) != nil {
		t.Error("a clean stop without a request is nil")
	}
}

func TestRestart758_ContainerOptionReachesTheAPI(t *testing.T) {
	c := &Container{}
	called := false
	WithRestartRequest(func() { called = true })(c)
	if c.restartRequest == nil {
		t.Fatal("WithRestartRequest did not set the container's restart request")
	}
	c.restartRequest()
	if !called {
		t.Fatal("the restart request was not the one given")
	}
}

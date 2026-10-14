package ctxerr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// audit 2026-10-12 (GitHub #142, outcome-quality LLD §S2): six call sites
// decided "cancel, deadline or upstream error" six different ways, and the
// differences were the defects — a cancel counted as a timeout, a deadline
// counted as a plain error. These cases pin the one rule they now share.
func TestClassify(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	live := context.Background()

	killed := errors.New("signal: killed") // what os/exec reports when it kills the child for ANY ctx end
	upstream := errors.New("502 bad gateway")

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want Cause
	}{
		{"nil err, live ctx", live, nil, None},
		{"nil ctx, nil err", nil, nil, None},
		{"upstream error, live ctx", live, upstream, Upstream},
		{"nil ctx, upstream error", nil, upstream, Upstream},
		{"wrapped Canceled, live ctx", live, fmt.Errorf("call: %w", context.Canceled), Cancelled},
		{"wrapped DeadlineExceeded, live ctx", live, fmt.Errorf("call: %w", context.DeadlineExceeded), Timeout},
		// The finished context explains an error that does not wrap it.
		{"killed child on cancelled ctx", cancelled, killed, Cancelled},
		{"killed child on expired ctx", expired, killed, Timeout},
		// A finished context with no error still says why the call ended.
		{"cancelled ctx, nil err", cancelled, nil, Cancelled},
		{"expired ctx, nil err", expired, nil, Timeout},
		// Cancel beats a co-wrapped deadline (the IsUpstreamInfraError rule).
		{"both wrapped", live, errors.Join(context.DeadlineExceeded, context.Canceled), Cancelled},
		// A live context does not turn an inner deadline into an upstream error.
		{"inner deadline, live outer ctx", live, fmt.Errorf("x: %w", context.DeadlineExceeded), Timeout},
		// A cancelled ctx outranks a wrapped deadline in err: the caller left.
		{"cancelled ctx, deadline err", cancelled, context.DeadlineExceeded, Cancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassify_RealExecKill(t *testing.T) {
	// The CLI provider case end to end: os/exec kills the child on cancel and
	// reports "signal: killed", which the old string classifier called a timeout.
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected the killed child to report an error")
	}
	if got := Classify(ctx, err); got != Cancelled {
		t.Fatalf("Classify(killed-on-cancel) = %q, want %q (err %v)", got, Cancelled, err)
	}
}

func TestCauseString(t *testing.T) {
	for c, want := range map[Cause]string{None: "", Cancelled: "cancelled", Timeout: "timeout", Upstream: "error"} {
		if string(c) != want {
			t.Fatalf("label %q, want %q", c, want)
		}
	}
}

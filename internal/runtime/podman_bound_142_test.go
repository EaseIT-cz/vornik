package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #142, outcome-quality LLD §S7 #8: every podman verb but `wait` ran
// with no bound of its own — Manager.defaultTimeout was set and never read —
// so a `podman run` pulling from a stalled registry held its task forever on
// an execution context with no deadline.

// boundRecorder is an execPodman seam: it records the deadline each verb's
// context carries, and blocks verbs listed in hang until their context ends.
type boundRecorder struct {
	deadlines map[string]time.Duration // verb → remaining time at call; -1 = none
	hang      map[string]bool
}

func (b *boundRecorder) exec(ctx context.Context, args []string) ([]byte, error) {
	verb := args[0]
	if d, ok := ctx.Deadline(); ok {
		b.deadlines[verb] = time.Until(d)
	} else {
		b.deadlines[verb] = -1
	}
	if b.hang[verb] {
		<-ctx.Done()
		return []byte(""), errors.New("signal: killed")
	}
	if verb == "run" {
		return []byte("cid-1\n"), nil
	}
	if verb == "wait" {
		return []byte("0\n"), nil
	}
	return []byte("[]"), nil
}

func newBoundManager(rec *boundRecorder) *Manager {
	return &Manager{
		podmanPath:     "/bin/false",
		logger:         zerolog.Nop(),
		defaultTimeout: 60 * time.Second,
		imageTimeout:   15 * time.Minute,
		execPodman:     rec.exec,
		imageLabelFunc: func(context.Context, string) (string, bool, error) { return "id", false, nil },
	}
}

// TestPodman_EveryVerbButWaitGetsABound — audit 2026-10-12, GitHub #142 §S7
// #8: the control verbs get defaultTimeout, run and pull get imageTimeout,
// and wait (bounded by the step timeout in WaitForExit) gets nothing added.
func TestPodman_EveryVerbButWaitGetsABound(t *testing.T) {
	rec := &boundRecorder{deadlines: map[string]time.Duration{}}
	m := newBoundManager(rec)
	ctx := context.Background()

	_ = m.StopContainer(ctx, "c", false)
	_ = m.RemoveContainer(ctx, "c", true)
	_, _ = m.InspectContainer(ctx, "c")
	_, _ = m.ListContainers(ctx, nil)
	_, _ = m.Logs(ctx, "c", 10)
	_ = m.pullAgentImage(ctx, "img")
	_, _ = m.podman(ctx, "run", "--detach", "img")
	_, _ = m.podman(ctx, "wait", "c")

	for _, verb := range []string{"stop", "rm", "inspect", "ps", "logs"} {
		d, seen := rec.deadlines[verb]
		if !seen {
			t.Fatalf("verb %s never ran", verb)
		}
		if d < 0 || d > 60*time.Second {
			t.Errorf("podman %s ran with remaining %v, want the 60s control bound", verb, d)
		}
	}
	for _, verb := range []string{"pull", "run"} {
		d := rec.deadlines[verb]
		if d < 14*time.Minute || d > 15*time.Minute {
			t.Errorf("podman %s ran with remaining %v, want the 15m image bound", verb, d)
		}
	}
	if d := rec.deadlines["wait"]; d != -1 {
		t.Errorf("podman wait got a bound of %v from the choke point; WaitForExit owns it", d)
	}
}

// TestPodman_ACallerDeadlineTighterThanTheBoundIsKept — the bound only ever
// shortens a call.
func TestPodman_ACallerDeadlineTighterThanTheBoundIsKept(t *testing.T) {
	rec := &boundRecorder{deadlines: map[string]time.Duration{}}
	m := newBoundManager(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.StopContainer(ctx, "c", false)
	if d := rec.deadlines["stop"]; d < 0 || d > 5*time.Second {
		t.Fatalf("stop ran with remaining %v, want the caller's 5s", d)
	}
}

// TestPodman_AHungVerbEndsAsATimeoutThatNamesTheBound — audit 2026-10-12,
// GitHub #142 §S7 #8: a hung podman ends at the bound, the error names the
// verb and the bound, and it wraps context.DeadlineExceeded so the executor
// classifies it as a timeout and does not retry it in-execution.
func TestPodman_AHungVerbEndsAsATimeoutThatNamesTheBound(t *testing.T) {
	rec := &boundRecorder{deadlines: map[string]time.Duration{}, hang: map[string]bool{"stop": true, "run": true}}
	m := newBoundManager(rec)
	m.defaultTimeout = 20 * time.Millisecond
	m.imageTimeout = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- m.StopContainer(context.Background(), "c", false) }()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stop error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "podman stop did not finish within 20ms") {
			t.Errorf("stop error %q does not name the verb and its bound", err)
		}
		// The step-outcome refiner reads this literal as a container kill.
		if strings.Contains(err.Error(), "signal: killed") {
			t.Errorf("stop error %q leaks os/exec's kill text", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("podman stop is still running: the choke point applied no bound")
	}

	go func() {
		_, err := m.StartContainer(context.Background(), &ContainerConfig{
			TaskID: "t", ProjectID: "p", Role: "r", Image: "img",
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("start error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "podman run did not finish within 20ms") {
			t.Errorf("start error %q does not name the verb and its bound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("podman run is still running: the choke point applied no bound")
	}
}

// TestPodman_ACallerCancelIsNotReportedAsTheBound — the caller's own end is
// not dressed up as the runtime's bound.
func TestPodman_ACallerCancelIsNotReportedAsTheBound(t *testing.T) {
	rec := &boundRecorder{deadlines: map[string]time.Duration{}, hang: map[string]bool{"stop": true}}
	m := newBoundManager(rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.StopContainer(ctx, "c", false)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("a caller cancel was reported as the runtime bound: %q", err)
	}
}

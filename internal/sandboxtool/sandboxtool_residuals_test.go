package sandboxtool

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/spawn"
)

// The S5b code reviews (review-20260926-4a42 and review-20260926-c245, both
// GREEN with residuals) left three runner gaps. Each test below fails on the
// code the reviews read.

// R1: /out was bounded only AFTER the run, so a tool could fill the host disk
// inside its timeout. Every run now caps a single file with RLIMIT_FSIZE, and
// the runner samples /out during the run and kills the container at the first
// sample over the cap.
func TestRun_OutputIsBoundedDuringTheRun(t *testing.T) {
	f := &fakePodman{
		produces: "ocr.txt", size: int(16*mib) + 1, writeFirst: true,
		block: make(chan struct{}), rmUnblocks: true, runErr: errors.New("exit status 137"),
	}
	r := newRunner(t, f, func(c *Config) {
		c.Timeouts = map[Feature]time.Duration{FeatureImageOCR: 3 * time.Second}
	})
	r.outputSample = 10 * time.Millisecond
	start := time.Now()
	_, err := r.Run(context.Background(), ocrSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeFailed || !strings.Contains(err.Error(), "during the run") {
		t.Fatalf("want an in-run output failure, got %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("the run must be stopped at the first sample over the cap, not at its timeout (took %v)", elapsed)
	}

	run := strings.Join(f.runCalls()[0].args, " ")
	if want := "--ulimit fsize=" + strconv.FormatInt(r.MaxOutput(FeatureImageOCR), 10); !strings.Contains(run, want) {
		t.Fatalf("argv must carry %q: %s", want, run)
	}
}

// R2: on a timeout the context kill ends the podman CLIENT, not the container
// conmon supervises, so OOMKilled was read from a container that could still
// be running. The runner stops it first.
func TestRun_TimeoutStopsTheContainerBeforeReadingItsVerdict(t *testing.T) {
	f := &fakePodman{block: make(chan struct{})}
	r := newRunner(t, f, func(c *Config) {
		c.Timeouts = map[Feature]time.Duration{FeatureRender: 30 * time.Millisecond}
	})
	_, err := r.Run(context.Background(), renderSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
	stopAt, inspectAt := -1, -1
	f.mu.Lock()
	for i, c := range f.calls {
		switch {
		case len(c.args) > 0 && c.args[0] == "stop" && stopAt < 0:
			stopAt = i
			if got := strings.Join(c.args, " "); !strings.Contains(got, "-t 0") {
				t.Errorf("stop must not wait for a grace period: %s", got)
			}
		case len(c.args) > 0 && c.args[0] == "inspect" && inspectAt < 0:
			inspectAt = i
		}
	}
	f.mu.Unlock()
	if stopAt < 0 || inspectAt < 0 || stopAt > inspectAt {
		t.Fatalf("want podman stop before podman inspect; stop at %d, inspect at %d", stopAt, inspectAt)
	}
}

// R3: a label re-read held the tool mutex across `podman image inspect` (up
// to 30 s), stalling every feature, voice included. A refresh now runs
// outside the lock: callers answer from the last read and never wait on it.
func TestToolDeclared_ARefreshNeverBlocksOtherCallers(t *testing.T) {
	onlyPDF := "pdftotext"
	f := &fakePodman{label: &onlyPDF}
	r := newRunner(t, f, nil)
	if _, err := r.LoadTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.now = func() time.Time { return now.Add(labelRefresh + time.Second) }

	f.mu.Lock()
	f.inspectBlock = make(chan struct{})
	f.mu.Unlock()
	defer close(f.inspectBlock)

	missDone := make(chan struct{})
	go func() {
		defer close(missDone)
		r.Declared(context.Background(), "tesseract") // a miss: triggers the blocked re-read
	}()
	waitFor(t, func() bool { return f.imageCalls() >= 2 })

	answered := make(chan bool, 1)
	go func() {
		ok, _ := r.Declared(context.Background(), "pdftotext")
		answered <- ok
	}()
	select {
	case ok := <-answered:
		if !ok {
			t.Fatal("pdftotext is declared by the last read")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller waited on another caller's label refresh")
	}
	select {
	case <-missDone:
		t.Fatal("the miss should still be refreshing")
	default:
	}
}

// R3, the first read: with no label read yet there is nothing to answer
// from, so a caller waits, but only as long as its own context allows.
func TestToolDeclared_TheFirstReadIsWaitedOnWithinTheCallersContext(t *testing.T) {
	f := &fakePodman{inspectBlock: make(chan struct{})}
	r := newRunner(t, f, nil)
	defer close(f.inspectBlock)

	go r.Declared(context.Background(), "pdftotext") // the first read, blocked
	waitFor(t, func() bool { return f.imageCalls() >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	ok, detail := r.Declared(ctx, "pdftotext")
	if ok || detail == "" {
		t.Fatalf("with no label read and the caller's context done: want not declared with a reason, got %v %q", ok, detail)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("the caller must return at its own deadline (took %v)", elapsed)
	}
}

func (f *fakePodman) imageCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "image" {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// S1b-2 merged onto the S5b residuals: the spawn grammar was written against a
// hand copy of this runner's argv, so --ulimit (R1) and `stop --ignore` (R2)
// were refused by real podman runs while every fake-runner test passed. The
// runner's own argv, and every control verb it sends, must parse.
func TestSpawnGrammar_AcceptsEveryArgvTheRunnerBuilds(t *testing.T) {
	var calls [][]string
	f := &fakePodman{block: make(chan struct{})}
	r := newRunner(t, f, func(c *Config) {
		c.Image = "localhost/vornik-agent:test"
		c.Timeouts = map[Feature]time.Duration{FeatureRender: 30 * time.Millisecond}
	})
	_, _ = r.Run(context.Background(), renderSpec()) // times out: stop, inspect, rm
	close(f.block)
	f.block = nil
	for _, feat := range Features() {
		calls = append(calls, r.argv(Spec{Feature: feat, Entrypoint: "tool", ModelDir: "/m", Stdin: "text",
			Args: []string{"/in/x", "/out/y"}}, "vornik-sbx-ab", "/a", "/b"))
	}
	if _, err := r.SweepContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for _, c := range f.calls {
		calls = append(calls, c.args)
	}
	f.mu.Unlock()
	for _, argv := range calls {
		var err error
		if argv[0] == "run" {
			_, err = spawn.PodmanAgent(context.Background(), "", argv)
		} else {
			_, err = spawn.PodmanControl(context.Background(), "", argv)
		}
		if err != nil {
			t.Errorf("the spawn grammar refuses the runner's own argv %q: %v", strings.Join(argv, " "), err)
		}
	}
}

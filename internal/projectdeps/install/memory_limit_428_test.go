package install

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// GitHub #428 (found at: review, lane W14-M's spawn-path audit for #379): the
// probe and the pip install ran in the agent image with no --memory and no
// --memory-swap, so a dependency install was bounded by nothing but the host
// and could fill host swap, the shape behind the 2026-10-14 OOM. Both now
// carry spawn.MemoryLimitFlags (project dependency provisioning design §8.6).

const limit428 int64 = 1 << 30 // 1 GiB

// memFlagsBeforeImage reports whether args carries exactly one
// --memory=N and one --memory-swap=N, both before the "--" that ends podman's
// options (after it they would be the image's arguments, not podman's).
func memFlagsBeforeImage(t *testing.T, args []string, n string) {
	t.Helper()
	end := -1
	for i, a := range args {
		if a == "--" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatalf("argv has no -- before the image: %v", args)
	}
	for _, want := range []string{"--memory=" + n, "--memory-swap=" + n} {
		count, at := 0, -1
		for i, a := range args {
			if a == want {
				count++
				at = i
			}
		}
		if count != 1 || at > end {
			t.Errorf("argv must carry %s exactly once, before --: %v", want, args)
		}
	}
}

func noMemFlags(t *testing.T, args []string) {
	t.Helper()
	for _, a := range args {
		if strings.HasPrefix(a, "--memory") {
			t.Errorf("an unbounded install must carry no memory flag, got %s in %v", a, args)
		}
	}
}

func TestProbe_CarriesTheMemoryLimit(t *testing.T) {
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){"run": probeAnswer("cpython-312 x86_64 glibc-2.39")}}
	if _, err := Probe(context.Background(), f.run, "vornik-agent:latest", limit428); err != nil {
		t.Fatal(err)
	}
	memFlagsBeforeImage(t, f.calls[0].args, "1073741824")

	none := &fakeRunner{answers: map[string]func([]string) ([]byte, error){"run": probeAnswer("cpython-312 x86_64 glibc-2.39")}}
	if _, err := ChooseInterpreter(context.Background(), none.run, []string{"vornik-agent:latest"}, 0); err != nil {
		t.Fatal(err)
	}
	noMemFlags(t, none.calls[0].args)
}

func TestChooseInterpreter_PassesTheLimitToEveryProbe(t *testing.T) {
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){"run": probeAnswer("cpython-312 x86_64 glibc-2.39")}}
	if _, err := ChooseInterpreter(context.Background(), f.run, []string{"a:1", "b:1"}, limit428); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want one probe per image, got %d", len(f.calls))
	}
	for _, c := range f.calls {
		memFlagsBeforeImage(t, c.args, "1073741824")
	}
}

func TestPipFetcher_CarriesTheMemoryLimit(t *testing.T) {
	for _, wheelhouse := range []string{"", "/mnt/wheels"} {
		f := &fakeRunner{}
		if err := PipFetcher(f.run, "vornik-agent:latest", "/srv/ws/proj/requirements.lock", wheelhouse, limit428)(context.Background(), "/cache/.staging-1"); err != nil {
			t.Fatal(err)
		}
		memFlagsBeforeImage(t, f.calls[0].args, "1073741824")

		none := &fakeRunner{}
		if err := PipFetcher(none.run, "vornik-agent:latest", "/srv/ws/proj/requirements.lock", wheelhouse, 0)(context.Background(), "/cache/.staging-1"); err != nil {
			t.Fatal(err)
		}
		noMemFlags(t, none.calls[0].args)
	}
}

func TestFormatMemory(t *testing.T) {
	for n, want := range map[int64]string{16 << 20: "16 MiB", 512 << 20: "512 MiB", 1 << 30: "1.0 GiB", 4110510080: "3.8 GiB"} {
		if got := FormatMemory(n); got != want {
			t.Errorf("FormatMemory(%d) = %q, want %q", n, got, want)
		}
	}
}

// exitErr stands in for *exec.ExitError: anything with ExitCode().
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func TestPipFetcher_KilledAtTheLimitSaysSo(t *testing.T) {
	run := func(code int) func(context.Context, string, ...string) ([]byte, error) {
		return func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Collecting numpy\n"), exitErr(code)
		}
	}
	err := PipFetcher(run(137), "vornik-agent:latest", "/p/requirements.lock", "", limit428)(context.Background(), "/cache/.staging-1")
	if err == nil {
		t.Fatal("a killed pip must fail")
	}
	for _, want := range []string{"killed (exit 137)", "memory limit of 1.0 GiB", "runtime.agent_memory_limit", "Collecting numpy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a pip killed at the limit must say %q, got: %v", want, err)
		}
	}
	var ec interface{ ExitCode() int }
	if !errors.As(err, &ec) || ec.ExitCode() != 137 {
		t.Errorf("the runner's error must stay wrapped, got %v", err)
	}

	// No bound applied: a 137 is not the limit's doing, so no limit message.
	err = PipFetcher(run(137), "img", "/p/requirements.lock", "", 0)(context.Background(), "/cache/.staging-1")
	if err == nil || strings.Contains(err.Error(), "memory limit") {
		t.Errorf("an unbounded 137 must not blame a limit: %v", err)
	}
	// A bound applied but pip failed on its own (exit 1): pip's message, no limit blame.
	err = PipFetcher(run(1), "img", "/p/requirements.lock", "", limit428)(context.Background(), "/cache/.staging-1")
	if err == nil || strings.Contains(err.Error(), "memory limit") {
		t.Errorf("an ordinary pip failure must not blame the limit: %v", err)
	}
}

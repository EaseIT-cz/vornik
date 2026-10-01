package macos

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestZeroEgressNetworkNoneCannotReachExternalHost encodes the zero-egress
// invariant the whole macOS design (§2) exists to preserve: a container
// started with --network=none has NO network device at all, so it cannot
// reach any external endpoint — the property that lets the device-free unix
// socket path (daemon<->agent) satisfy zero-egress on Linux, and,
// byte-for-byte, inside the Lima VM (design §2, §3).
//
// IMPORTANT SCOPE NOTE: this test verifies the LINUX invariant only — it runs
// a plain rootless-podman container on whatever host executes `go test`. The
// macOS side of this design (the Lima VM, its virtiofs config-only mount, the
// host<->VM boundary in §2) is NOT exercised here and has no automated
// coverage; per design §6 it is manual-smoke-only ("needs a Mac"). A green
// run of this test is Linux zero-egress coverage, NOT macOS coverage — do
// not read it as verifying the Lima bind-mount chain.
func TestZeroEgressNetworkNoneCannotReachExternalHost(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available on this host — skipping (see deployments/podman/ for the CI lane that has it)")
	}

	// A small, already-common image with a built-in HTTP client (busybox
	// wget) so the test needs no extra tooling inside the container beyond
	// what --network=none is checking.
	const image = "docker.io/library/alpine:3.19"
	if err := exec.Command("podman", "image", "exists", image).Run(); err != nil {
		if pullErr := exec.Command("podman", "pull", image).Run(); pullErr != nil {
			t.Skipf("could not obtain %s (no image cached, pull failed: %v) — skipping", image, pullErr)
		}
	}

	wgetArgs := []string{"wget", "-T", "3", "-q", "-O-", "http://1.1.1.1"}

	// Positive control: the SAME image/command with default (bridge)
	// networking must SUCCEED first. Without this, any non-zero exit from
	// the --network=none run below would false-pass the test even if it
	// failed for an unrelated reason (bad flags, broken storage, a bad
	// image) rather than because of the --network=none isolation itself.
	// A skip here (not a failure) since a sandboxed/offline CI runner
	// legitimately can't reach the network either way — that's an
	// environment limitation, not a signal about the invariant under test.
	if err := runWithTimeout(t, 15*time.Second, "podman", append([]string{"run", "--rm", image}, wgetArgs...)...); err != nil {
		t.Skipf("positive control failed (podman run with default networking could not reach http://1.1.1.1: %v) — "+
			"skipping: this host/CI runner may itself be network-isolated, or podman/image tooling is unhealthy; "+
			"either way the --network=none run below would prove nothing", err)
	}

	// The actual invariant: the same image/command, but with --network=none,
	// must FAIL — having just proven (above) that the image and command work
	// and can reach the network when a device is present.
	err := runWithTimeout(t, 15*time.Second, "podman", append([]string{"run", "--rm", "--network=none", image}, wgetArgs...)...)
	if errors.Is(err, errCommandTimeout) {
		t.Fatalf("%v — a --network=none container should be refused at once, not hang", err)
	}
	if err == nil {
		t.Fatal("container with --network=none reached an external host — the zero-egress invariant is broken " +
			"(no network device should mean no route to anywhere, external or not)")
	}
	// Non-zero exit (typically wget's "bad address"/network-unreachable for
	// a device-less netns) is the expected, immediate failure — already
	// distinguished from an unrelated tooling failure by the positive
	// control above having succeeded.
}

// runWithTimeout runs cmd and returns its error (nil on success). A command
// that does not return within timeout is killed and reported as
// errCommandTimeout, so a hung podman invocation never hangs the suite and the
// caller decides what a hang means.
var errCommandTimeout = errors.New("did not return within its limit")

func runWithTimeout(t *testing.T, timeout time.Duration, name string, arg ...string) error {
	t.Helper()
	cmd := exec.Command(name, arg...)
	// Start before the goroutine: Kill reads cmd.Process, which Start sets,
	// and a Run inside the goroutine raced with it (-race, 2026-10-01).
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return fmt.Errorf("%s %v: %w (%s)", name, arg, errCommandTimeout, timeout)
	}
}

// A command that outlives its limit is reported as errCommandTimeout, so the
// caller decides: the positive control skips (a slow or unreachable network
// proves nothing), the --network=none run fails. Before 2026-10-01 a hung
// positive control failed the test (the CE export's unit lane hit one).
func TestRunWithTimeoutReturnsTimeoutToTheCaller(t *testing.T) {
	err := runWithTimeout(t, 100*time.Millisecond, "sleep", "5")
	if !errors.Is(err, errCommandTimeout) {
		t.Fatalf("err = %v, want errCommandTimeout", err)
	}
	if err := runWithTimeout(t, 5*time.Second, "true"); err != nil {
		t.Fatalf("a quick command: %v", err)
	}
}

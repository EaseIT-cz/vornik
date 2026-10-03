package hostdoctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// hostUID returns the daemon's own uid, mirroring what checkAgentImageUID
// compares the agent image's baked uid against.
func hostUID() int {
	return os.Getuid()
}

// Regression: rootless workspace "Permission denied" second guard — a raw
// `podman build` bakes uid 1000; keep-id can't bridge a baked-uid mismatch
// (verified 2026-07-25). See onboarding-hardening-design F3b.
func TestCheckAgentImageUID(t *testing.T) {
	host := hostUID() // helper returns os.Getuid()

	// h builds a Checker backed by a real configDir with one
	// role referencing a real (non-noop) agent image, so firstAgentImage
	// resolves unconditionally and the injected bakedUIDFunc/subuidOKFunc
	// seams are what actually determine the outcome.
	h := func(t *testing.T) *Checker {
		t.Helper()
		dir := t.TempDir()
		writeSwarmWithImage(t, dir, "vornik-agent:latest")
		return &Checker{
			configDir: dir, usernsMode: "keep-id", subuidOKFunc: func() bool { return true },
			// Unlabelled, and injected: this test used to fall through to
			// realImageLabels and shell out to podman (found 2026-10-03, D5).
			imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
				return map[string]string{}, nil
			},
			hostIdentityFunc: func() (int, int, bool) { return host, host, true },
		}
	}

	// baked == host -> OK
	d := h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host, nil }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "OK" {
		t.Fatalf("baked==host -> OK, got %q", got.Status)
	}
	// baked != host -> ERROR
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host + 7, nil }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "ERROR" {
		t.Fatalf("baked!=host -> ERROR, got %q", got.Status)
	}
	// podman error -> SKIPPED (was WARNING until 2026-09-18).
	//
	// Changed deliberately, not incidentally. The old assertion pinned the
	// original code's behaviour and carried no rationale; it predates both
	// 2026-08-26-doctor-skipped-vs-ok-design.md §E2 — "the check could not run
	// at all → SKIPPED … never WARNING. A driver error is never a verdict" —
	// and CE issue 59, where a probe that could not finish inside its deadline
	// reported "could not read agent image uid: signal: killed" as a WARNING on
	// every run of a HEALTHY deployment. SKIPPED is already excluded from the
	// issue count, which is the behaviour that case needs.
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return 0, errors.New("boom") }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "SKIPPED" {
		t.Fatalf("podman error -> SKIPPED, got %q", got.Status)
	}
	// keep-id set but subuid missing -> ERROR (preflight)
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host, nil }
	d.subuidOKFunc = func() bool { return false }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "ERROR" {
		t.Fatalf("missing subuid -> ERROR, got %q", got.Status)
	}
}

// No agent image configured (empty configDir, no swarms) and no bakedUIDFunc
// override -> SKIPPED. Proves the image-absent branch is actually reachable
// now that resolution isn't gated behind whether a test seam is injected.
func TestCheckAgentImageUID_NoImageConfigured_Skipped(t *testing.T) {
	dir := t.TempDir() // empty: no swarms/ dir at all
	d := &Checker{configDir: dir}
	got := d.checkAgentImageUID(context.Background())
	if got.Status != "SKIPPED" {
		t.Fatalf("no agent image configured -> SKIPPED, got %q (%s)", got.Status, got.Message)
	}
}

// keep-id + missing subuid provisioning must fire even when configDir is
// empty — the preflight is a host-level prerequisite check, not gated on
// any config directory being set.
func TestCheckAgentImageUID_KeepIDPreflight_RunsBeforeConfigDirGuard(t *testing.T) {
	d := &Checker{
		configDir:    "",
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return false },
	}
	got := d.checkAgentImageUID(context.Background())
	if got.Status != "ERROR" {
		t.Fatalf("keep-id missing subuid with empty configDir -> ERROR, got %q (%s)", got.Status, got.Message)
	}
}

// 2026-09-18: the agent image became uid-agnostic (onboarding-hardening-design
// D4) — /home/vornik, the Go cache tree and the contract mount points are 1777,
// so the image works whatever uid the process runs as. Measured: a uid-1000
// image running as uid 1001 completes a real `go build`.
//
// That falsified this check. It compared baked uid to host uid and, on a
// mismatch, reported ERROR "keep-id cannot bridge this and rootless workspace
// writes will fail. Rebuild with make build-agent" — a failure that no longer
// happens and a remedy the design calls a tax the next `podman pull` undoes.
//
// The image now says so itself with a label, which also removes the container
// start that CE issue 59 showed takes 5.92-12.55s and gets SIGKILLed by the
// probe's own deadline.

// A labelled image is fine whatever its baked uid: that is the point of D4.
//
// Changed 2026-10-03 (D5): this test asserted OK from the label ALONE, which is
// the verdict the doctor gave on the 2026.10.3 reference host while every agent
// step failed. Since D5 the label path is OK only once the probe AS THE
// RESOLVED USER (daemon uid:gid, keep-id) passes (R6). The baked-uid probe
// (CE issue 59's slow start) still does not run for a labelled image.
func TestCheckAgentImageUID_UIDAgnosticLabelIsOKDespiteMismatch(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	bakedProbed := false
	var labelProbedAs []string
	h := &Checker{
		configDir:        dir,
		usernsMode:       "keep-id",
		subuidOKFunc:     func() bool { return true },
		hostIdentityFunc: func() (int, int, bool) { return 1001, 1001, true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return map[string]string{agentUIDAgnosticLabel: "1"}, nil
		},
		labelProbeFunc: func(_ context.Context, _ string, user string) (bool, string, error) {
			labelProbedAs = append(labelProbedAs, user)
			return true, "", nil
		},
		bakedUIDFunc: func(context.Context, string) (int, error) {
			bakedProbed = true
			return 1000, nil
		},
	}

	got := h.checkAgentImageUID(context.Background())

	if got.Status != "OK" {
		t.Errorf("status = %q (%s), want OK — a uid-agnostic image works at any uid",
			got.Status, got.Message)
	}
	if bakedProbed {
		t.Error("the baked-uid probe ran even though the label answered — this is the " +
			"start that CE issue 59 reports taking 5.92-12.55s and being SIGKILLed")
	}
	if len(labelProbedAs) != 1 || labelProbedAs[0] != "1001:1001" {
		t.Errorf("label probe ran as %v, want exactly [1001:1001] (D5 R6)", labelProbedAs)
	}
}

// An UNLABELLED image with a mismatch is genuinely broken — that is an image
// built before D4 — so the error stays, and it must stay an error.
func TestCheckAgentImageUID_UnlabelledMismatchIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	h := &Checker{
		configDir:    dir,
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"org.opencontainers.image.version": "2026.9.4"}, nil
		},
		bakedUIDFunc: func(context.Context, string) (int, error) { return hostUID() + 1, nil },
	}

	got := h.checkAgentImageUID(context.Background())

	if got.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR for a pre-D4 image whose uid does not match", got.Status)
	}
}

// A probe that cannot complete must report NOT MEASURED, distinctly from a real
// mismatch. CE issue 59: the check reported "could not read agent image uid:
// signal: killed" as a WARNING on every run, which reads like a finding about
// the image and is actually a finding about the probe.
func TestCheckAgentImageUID_UnreadableProbeSaysNotMeasured(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	h := &Checker{
		configDir:    dir,
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return nil, errors.New("no such image")
		},
		bakedUIDFunc: func(context.Context, string) (int, error) {
			return 0, errors.New("signal: killed")
		},
	}

	got := h.checkAgentImageUID(context.Background())

	// SKIPPED is the established vocabulary for "the check could not run at
	// all" — 2026-08-26-doctor-skipped-vs-ok-design.md §E2 — and it is already
	// excluded from the issue count, which WARNING is not.
	if got.Status != "SKIPPED" {
		t.Errorf("status = %q (%s), want SKIPPED — a probe that could not complete "+
			"is a finding about the probe, not about the image", got.Status, got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Message), "not measured") {
		t.Errorf("message = %q, want it to say the uid was NOT MEASURED", got.Message)
	}
}

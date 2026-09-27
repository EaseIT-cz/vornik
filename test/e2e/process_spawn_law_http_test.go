//go:build e2e_http
// +build e2e_http

// Process-spawn law, S2 — black-box e2e on the REAL binaries
// (https://docs.vornik.io).
//
// The operator ruling (2026-08-03): only vornikctl may spawn processes. S2 took
// the doctor's host checks (podman, skopeo, systemctl, git), the support
// report's `podman info` and control-plane diagnose's `journalctl` off the
// daemon's request path. These tests prove both halves on the booted daemon:
//
//   - NO SPAWN: the host programs are replaced, first on the daemon's PATH, by
//     shims that log every call. The requests must add nothing to that log.
//     A daemon that still spawned would log a line — the shims are what make
//     this discriminate, where "the response looks fine" would not.
//   - STILL WORKS: the same capability is still delivered — the daemon reports
//     its own checks plus a host_checks pointer, and the real vornikctl binary
//     runs the host checks locally (they do reach the shims: vornikctl is the
//     process allowed to spawn) and prints one merged report.
//
// Diagnose is Enterprise-gated; its test is in
// process_spawn_law_diagnose_ee_test.go, which the CE export leaves out.
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// spawnShimPrograms are the host programs the daemon's request path must not
// run. git is not shimmed: the daemon owns task worktrees (the orchestration
// allowlist), and its startup uses it.
var spawnShimPrograms = []string{"podman", "skopeo", "systemctl", "journalctl"}

// installSpawnShims writes the logging stand-ins. Each exits 1 — what the
// daemon sees on a host without the program — except `podman version`, which
// the daemon's runtime (orchestration, the allowlist) requires to start.
func installSpawnShims(work string) error {
	spawnShimDir = filepath.Join(work, "spawn-shims")
	spawnShimLog = filepath.Join(work, "spawn-shims.log")
	if err := os.MkdirAll(spawnShimDir, 0o755); err != nil {
		return err
	}
	for _, prog := range spawnShimPrograms {
		script := fmt.Sprintf("#!/bin/sh\necho \"%s $*\" >> '%s'\n", prog, spawnShimLog)
		if prog == "podman" {
			script += "if [ \"$1\" = version ]; then echo '{\"Client\":{\"Version\":\"5.0.0\"},\"Server\":{\"Version\":\"5.0.0\"}}'; exit 0; fi\n"
		}
		script += "exit 1\n"
		if err := os.WriteFile(filepath.Join(spawnShimDir, prog), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// hostCheckSpawn reports whether a logged call is one of the host checks S2
// moved off the daemon: the doctor's podman runtime/image probes, skopeo, the
// systemd and compose probes, and the journal tail. The daemon's own
// orchestration (podman version, ps/inspect of its containers) is allowed and
// may run in the background at any time, so it is not counted.
func hostCheckSpawn(line string) bool {
	switch {
	case strings.HasPrefix(line, "skopeo "), strings.HasPrefix(line, "systemctl "), strings.HasPrefix(line, "journalctl "):
		return true
	case strings.HasPrefix(line, "podman info"),
		strings.HasPrefix(line, "podman image "),
		strings.HasPrefix(line, "podman run --rm --entrypoint id"),
		strings.Contains(line, "com.docker.compose.project.config_files"):
		return true
	}
	return false
}

func hostCheckSpawns(lines []string) []string {
	var out []string
	for _, l := range lines {
		if hostCheckSpawn(l) {
			out = append(out, l)
		}
	}
	return out
}

func shimLogLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(spawnShimLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// spawnsDuring returns the host-program calls logged while fn ran.
func spawnsDuring(t *testing.T, fn func()) []string {
	t.Helper()
	before := len(shimLogLines(t))
	fn()
	return shimLogLines(t)[before:]
}

type e2eDoctorReport struct {
	Checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"checks"`
	Summary string `json:"summary"`
}

func (r e2eDoctorReport) names() map[string]bool {
	out := map[string]bool{}
	for _, c := range r.Checks {
		out[c.Name] = true
	}
	return out
}

var movedHostChecks = []string{"podman_config", "agent_images", "agent_image_uid", "image_freshness"}

func TestSpawnLaw_DaemonDoctorRunsNoHostProgram(t *testing.T) {
	for _, q := range []string{"", "?fix=true"} {
		var body string
		spawned := spawnsDuring(t, func() {
			var resp *http.Response
			resp, body = doReq(t, http.MethodPost, "/api/v1/doctor"+q, "", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /api/v1/doctor%s = %d: %s", q, resp.StatusCode, body)
			}
		})
		if hs := hostCheckSpawns(spawned); len(hs) > 0 {
			t.Errorf("POST /api/v1/doctor%s ran host checks on the daemon: %q", q, hs)
		}
		var report e2eDoctorReport
		if err := json.Unmarshal([]byte(body), &report); err != nil {
			t.Fatalf("doctor report: %v\n%s", err, body)
		}
		names := report.names()
		if !names["host_checks"] {
			t.Errorf("the report must point to the host checks: %v", names)
		}
		for _, moved := range movedHostChecks {
			if names[moved] {
				t.Errorf("the daemon still runs %s", moved)
			}
		}
		// Still works: the daemon's own checks are all there.
		for _, own := range []string{"stale_leases", "config_validation", "database_schema", "orphan_worktrees"} {
			if !names[own] {
				t.Errorf("the daemon's own check %s is missing: %v", own, names)
			}
		}
	}
}

func TestSpawnLaw_SupportReportRunsNoHostProgram(t *testing.T) {
	spawned := spawnsDuring(t, func() {
		// Still works: the bundle is built. (Its body may be compressed, so
		// the spawn log, not a substring, is the evidence below.)
		resp, body := doReq(t, http.MethodPost, "/api/v1/support-report", "application/json", strings.NewReader(`{"since":"1h"}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/v1/support-report = %d: %s", resp.StatusCode, body)
		}
	})
	if hs := hostCheckSpawns(spawned); len(hs) > 0 {
		t.Errorf("the support report ran host checks on the daemon: %q", hs)
	}
}

// --- vornikctl: the host checks still run, on the host, merged ---

var (
	vornikctlOnce sync.Once
	vornikctlBin  string
	vornikctlErr  error
)

func buildVornikctl(t *testing.T) string {
	t.Helper()
	vornikctlOnce.Do(func() {
		vornikctlBin = filepath.Join(e2eWork, "vornikctl")
		build := exec.Command("go", "build", "-o", vornikctlBin, "./cmd/vornikctl")
		build.Dir = repoRoot()
		out, err := build.CombinedOutput()
		if err != nil {
			vornikctlErr = fmt.Errorf("build vornikctl: %w\n%s", err, out)
		}
	})
	if vornikctlErr != nil {
		t.Fatal(vornikctlErr)
	}
	return vornikctlBin
}

// runVornikctl runs the real CLI against the booted daemon, with the shims
// first on ITS path so its host checks are deterministic on any host. A doctor
// that finds issues exits 1, which is not a failure of the command.
func runVornikctl(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(buildVornikctl(t), args...)
	cmd.Env = append(os.Environ(),
		"VORNIK_API_URL="+httpBase,
		"VORNIK_CONFIG="+e2eConfigFile,
		"VORNIK_CONFIGS_DIR="+e2eConfigsTree,
		"PATH="+spawnShimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("vornikctl %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
		}
	}
	return stdout.String()
}

func TestSpawnLaw_VornikctlDoctorJSONMergesTheHostChecks(t *testing.T) {
	var out string
	cliSpawns := spawnsDuring(t, func() { out = runVornikctl(t, "doctor", "--json") })
	var report e2eDoctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("vornikctl doctor --json is not JSON: %v\n%s", err, out)
	}
	names := report.names()
	for _, moved := range movedHostChecks {
		if !names[moved] {
			t.Errorf("vornikctl doctor --json lacks the host check %s: %v", moved, names)
		}
	}
	if names["host_checks"] {
		t.Error("the pointer must be replaced by the host checks, not printed")
	}
	if !names["stale_leases"] || !names["database_schema"] {
		t.Errorf("the daemon's own checks are missing from the merged report: %v", names)
	}
	// The host checks really ran, from vornikctl (the one process the ruling
	// allows to spawn): its podman calls reached the shims.
	sawPodmanInfo := false
	for _, l := range cliSpawns {
		if strings.HasPrefix(l, "podman info") {
			sawPodmanInfo = true
		}
	}
	if !sawPodmanInfo {
		t.Errorf("vornikctl did not run the podman host check; calls: %q", cliSpawns)
	}
}

func TestSpawnLaw_VornikctlDoctorPrintsTheMergedReport(t *testing.T) {
	out := runVornikctl(t, "doctor")
	for _, want := range append([]string{"stale_leases", "database_schema"}, movedHostChecks...) {
		if !strings.Contains(out, want) {
			t.Errorf("vornikctl doctor output lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "host_checks") {
		t.Errorf("the pointer must be replaced, not printed:\n%s", out)
	}
}

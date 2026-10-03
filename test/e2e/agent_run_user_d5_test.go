//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"vornik.io/vornik/internal/runtime"
)

// publishedAgentImageD5 is the PUBLISHED agent image, pinned by its multi-arch
// index digest, never :latest (onboarding-hardening-design.md D5 R8): release
// 2026.10.3, built by CI with the Containerfile default uid 1000, carrying
// io.vornik.agent.uid-agnostic. It is the image the 2026.10.3 incident pulled.
const publishedAgentImageD5 = "ghcr.io/easeit-cz/vornik-agent@sha256:c5260e43ed8e332ddaa7d1e27547781d9267b0ed76af903bd1f47e7fd80a43df"

// TestPublishedImage_D5_RunAsUserEmptyUnderKeepIDRunsAsDaemonUID is D5's
// e2e case. The 2026.10.3 deploy on a uid-1001 host (userns_mode keep-id,
// run_as_user "") pulled this image; with no --user the agent ran as its baked
// uid 1000 and died "contract mount unusable: cannot read
// /app/input/task.json (running as uid:gid 1000:1000)".
//
// It drives the REAL runtime.Manager (run_as_user empty, keep-id) and asserts
// the agent runs as the daemon's uid, reads the 0700 contract, writes its
// output, and that the output is owned on the host by the daemon's uid (a
// subordinate-uid owner is exactly the failure keep-id prevents).
//
// The lane must run on a host whose uid is NOT the image's baked uid, or it
// would pass vacuously; the first step asserts that and FAILS, not skips.
// GitHub's ubuntu runners run as uid 1001 against this image's 1000.
func TestPublishedImage_D5_RunAsUserEmptyUnderKeepIDRunsAsDaemonUID(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("requires Linux + podman")
	}
	requirePodman(t)
	if os.Geteuid() == 0 {
		t.Fatal("this lane must run rootless: under rootful podman the keep-id rule does not apply (D5 R1)")
	}
	daemonUID, daemonGID := os.Getuid(), os.Getgid()

	if out, err := exec.Command("podman", "pull", publishedAgentImageD5).CombinedOutput(); err != nil {
		t.Fatalf("podman pull %s: %v\n%s", publishedAgentImageD5, err, out)
	}

	// Step 1: the host uid must differ from the image's baked uid. `id -u` run
	// inside the image is authoritative; Config.User is logged beside it.
	idOut, err := exec.Command("podman", "run", "--rm", "--entrypoint", "id", publishedAgentImageD5, "-u").CombinedOutput()
	if err != nil {
		t.Fatalf("baked uid probe: %v\n%s", err, idOut)
	}
	bakedUID, err := strconv.Atoi(strings.TrimSpace(string(idOut)))
	if err != nil {
		t.Fatalf("baked uid probe output %q: %v", idOut, err)
	}
	cfgUser, _ := exec.Command("podman", "image", "inspect", "--format", "{{.Config.User}}", publishedAgentImageD5).CombinedOutput()
	t.Logf("image baked uid (id -u) = %d, Config.User = %q, daemon uid = %d", bakedUID, strings.TrimSpace(string(cfgUser)), daemonUID)
	if bakedUID == daemonUID {
		t.Fatalf("host uid %d equals the image's baked uid: this lane would pass vacuously (D5 R8, review 44bf F4)", daemonUID)
	}

	m, err := runtime.New(runtime.WithUserNSMode("keep-id"))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	scratch := t.TempDir()
	inputDir := filepath.Join(scratch, "input")
	outputDir := filepath.Join(scratch, "output")
	workspaceDir := filepath.Join(scratch, "workspace")
	for _, d := range []string{inputDir, outputDir, workspaceDir} {
		// 0700, as the daemon creates them: only the daemon's uid can use them.
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(inputDir, "task.json"), []byte(cannedTaskJSON), 0o600); err != nil {
		t.Fatalf("write task.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "CANCEL"), []byte("1"), 0o600); err != nil {
		t.Fatalf("write CANCEL: %v", err)
	}
	config := func(task string) *runtime.ContainerConfig {
		return &runtime.ContainerConfig{
			Image: publishedAgentImageD5, ProjectID: "e2e-d5", Role: "coder", TaskID: task,
			InputDir: inputDir, OutputDir: outputDir, WorkspaceDir: workspaceDir,
			EnvVars: map[string]string{"VORNIK_LLM_ENDPOINT": "http://127.0.0.1:1", "VORNIK_LLM_MODEL": "noop"},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)

	// Step 2: `id -u` inside a container started with exactly the runtime's
	// keep-id argv (the entrypoint swapped for `id`, detached swapped for --rm).
	args, err := m.RunArgsForTest(ctx, config("d5-id-"+stamp), "keep-id")
	if err != nil {
		t.Fatalf("RunArgsForTest: %v", err)
	}
	if n := strings.Count(" "+strings.Join(args, " ")+" ", " --user "); n != 1 {
		t.Fatalf("runtime argv carries --user %d times, want 1: %v", n, args)
	}
	probe := make([]string, 0, len(args)+4)
	for _, a := range args[:len(args)-1] {
		if a == "--detach" {
			a = "--rm"
		}
		probe = append(probe, a)
	}
	probe = append(probe, "--entrypoint", "id", args[len(args)-1], "-u")
	out, err := exec.CommandContext(ctx, "podman", probe...).CombinedOutput()
	if err != nil {
		t.Fatalf("id -u under the runtime's argv: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(daemonUID) {
		t.Fatalf("id -u inside the agent = %q, want the daemon uid %d (gid %d)", got, daemonUID, daemonGID)
	}

	// Step 3: the real start: contract read, output written.
	id, err := m.StartContainer(ctx, config("d5-run-"+stamp))
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	t.Cleanup(func() { _ = m.RemoveContainer(context.Background(), id, true) })
	code, err := m.WaitForExit(ctx, id, agentRunTimeout)
	logs, _ := m.Logs(context.Background(), id, 50)
	if err != nil {
		t.Fatalf("WaitForExit: %v\n%s", err, logs)
	}
	resultPath := filepath.Join(outputDir, "result.json")
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result.json missing (exit %d): %v\n%s", code, err, logs)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil || result.Status != "CANCELLED" {
		t.Fatalf("result.json = %s (err %v), want status CANCELLED — the contract was not read\n%s", resultBytes, err, logs)
	}

	// Step 4: the output is the daemon's on the host (review 4ab9 F6).
	fi, err := os.Stat(resultPath)
	if err != nil {
		t.Fatalf("stat result.json: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no owner information for result.json")
	}
	if int(st.Uid) != daemonUID {
		t.Fatalf("result.json is owned on the host by uid %d, want the daemon uid %d", st.Uid, daemonUID)
	}
}

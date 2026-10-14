//go:build e2e_hermes

package hermes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/testutil/testenv"
)

// Pins (lane design §3). Changing one is a deliberate change that runs the
// lane.
const (
	hermesRepo       = "https://github.com/NousResearch/hermes-agent.git"
	hermesCommit     = "f97608f178d1ffeca59860195ab7da295f7c8e5f" // v2026.9.24
	hermesImage      = "localhost/vornik-e2e-hermes:f97608f178d1"
	llamaImage       = "ghcr.io/ggml-org/llama.cpp:server"
	pgvectorImage    = "docker.io/pgvector/pgvector:pg16"
	embeddingDim     = 384
	scriptedModel    = "e2e-scripted"
	rawMailSentinel  = "SENTINEL-RAWMAIL-7f3c1e"
	invoiceMessageID = "msg-invoice-1"
)

// pickModel returns the model this run uses.
func pickModel(t *testing.T) laneModel {
	t.Helper()
	id := os.Getenv("VORNIK_E2E_HERMES_LLM")
	if id == "" {
		id = defaultLaneModel
	}
	m, ok := laneModels[id]
	if !ok {
		t.Fatalf("VORNIK_E2E_HERMES_LLM=%q is not a pinned model", id)
	}
	return m
}

// repoRoot is the module root, found from this file's path.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}

func cacheDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("VORNIK_E2E_CACHE"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".cache", "vornik-e2e")
}

// run executes a command and fails the test with its output on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = testenv.Clean()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(string(out), 4000))
	}
	return string(out)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// requirePodman fails (not skips) when podman is missing: a run that did
// not happen is never reported green (lane design §6).
func requirePodman(t *testing.T) {
	t.Helper()
	if os.Getenv("VORNIK_E2E_HERMES_SKIP") == "1" {
		t.Skip("VORNIK_E2E_HERMES_SKIP=1: the Hermes lane was explicitly skipped")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatal("the Hermes lane needs podman on PATH (set VORNIK_E2E_HERMES_SKIP=1 to skip it explicitly)")
	}
}

func imageExists(ref string) bool {
	return exec.Command("podman", "image", "exists", ref).Run() == nil
}

// ensureModel downloads the pinned model once and checks its hash every run.
func ensureModel(t *testing.T, m laneModel) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, m.File)
	if _, err := os.Stat(path); err != nil {
		t.Logf("downloading %s (first run)", m.URL)
		run(t, "curl", "-fL", "--retry", "3", "-o", path+".part", m.URL)
		if err := os.Rename(path+".part", path); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// The container's memory limit is sized from this size (lane design,
	// "Memory guardrails"), so the basis is checked every run.
	if st, err := f.Stat(); err != nil || st.Size() != m.FileBytes {
		t.Fatalf("%s is not %d bytes (%v): FileBytes and the memory limit are sized from the pinned file", path, m.FileBytes, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		t.Fatalf("%s has sha256 %s, want %s: delete it to re-download", path, got, m.SHA256)
	}
	return dir
}

// ensureHermesImage builds Hermes from the pinned commit once, with the
// recorded podman patch (lane design, As built).
func ensureHermesImage(t *testing.T) {
	t.Helper()
	if imageExists(hermesImage) {
		return
	}
	src := filepath.Join(cacheDir(t), "hermes-src-"+hermesCommit[:12])
	if _, err := os.Stat(filepath.Join(src, "Dockerfile")); err != nil {
		t.Logf("fetching Hermes %s (first run)", hermesCommit)
		run(t, "git", "init", "-q", src)
		run(t, "git", "-C", src, "fetch", "-q", "--depth", "1", hermesRepo, hermesCommit)
		run(t, "git", "-C", src, "checkout", "-q", "FETCH_HEAD")
		run(t, "git", "-C", src, "apply", filepath.Join(repoRoot(t), "test", "e2e", "hermes", "hermes-podman.patch"))
	}
	t.Logf("building %s (first run; ~20 minutes)", hermesImage)
	run(t, "podman", "build", "-t", hermesImage, src)
}

func ensureImage(t *testing.T, ref string) {
	t.Helper()
	if !imageExists(ref) {
		run(t, "podman", "pull", ref)
	}
}

// reapStaleContainers removes lane containers that have exited or are older
// than reapMaxAge. It runs once, before the first startContainer.
func reapStaleContainers() {
	out, err := exec.Command("podman", reaperListArgs()...).Output()
	if err != nil {
		return
	}
	var cs []podmanContainer
	if json.Unmarshal(out, &cs) != nil {
		return
	}
	if ids := reapCandidates(cs, time.Now()); len(ids) > 0 {
		_ = exec.Command("podman", append([]string{"rm", "-f"}, ids...)...).Run()
	}
}

// requireMemory is the memory preflight for one stage (lane design, "Memory
// guardrails", incident 2026-10-14): it fails the test, never skips, when the
// host cannot hold what the stage adds plus the margin, unless
// VORNIK_E2E_DEDICATED_HOST=1, which logs the finding as a warning.
func requireMemory(t *testing.T, stage string, items ...memItem) {
	t.Helper()
	f, err := os.Open("/proc/meminfo")
	var h hostMemory
	if err == nil {
		h, err = readHostMemory(f)
		_ = f.Close()
	}
	override := os.Getenv(dedicatedHostEnv) == "1"
	var foreign []string
	var foreignErr error
	if override {
		// The override holds only on a host running nothing but this lane.
		out, perr := exec.Command("podman", "ps", "--format", "json").Output()
		var cs []podmanContainer
		if perr == nil {
			perr = json.Unmarshal(out, &cs)
		}
		if perr != nil {
			foreignErr = fmt.Errorf("podman ps: %w", perr)
		} else {
			// The lane daemons' own agent containers are the lane's;
			// "cannot tell" is foreign (amendment W8-AJ).
			foreign = foreignContainers(cs, laneTaskOwner(directClient, registeredLaneDaemons()))
		}
	}
	v := decideMemory(stage, h, err, items, override, foreign, foreignErr)
	if !v.OK {
		t.Fatal(v.Message)
	}
	if v.Overridden {
		t.Log(v.Message)
	}
}

// hermesRunArgs is the argv for one container of the Hermes image, after
// that container's preflight.
func hermesRunArgs(t *testing.T, args ...string) []string {
	t.Helper()
	requireMemory(t, "before a Hermes container", hermesStageItems()...)
	return podmanRunArgs(memHermes, args...)
}

// startContainer runs a detached container, limited to limit bytes with no
// swap, and removes it at cleanup.
func startContainer(t *testing.T, name string, limit int64, args ...string) {
	t.Helper()
	// No "rm -f <name>": names are per run, and a fixed-name rm removed a
	// live sibling's container (T10). Leftovers are reaped by label instead.
	run(t, "podman", podmanRunArgs(limit, append([]string{"-d", "--name", name}, args...)...)...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("podman", "logs", "--tail", "80", name).CombinedOutput()
			t.Logf("--- %s logs ---\n%s", name, logs)
		}
		_ = exec.Command("podman", "rm", "-f", name).Run()
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// waitFor polls until ok returns true or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// directClient talks to localhost without any proxy the environment sets.
var directClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}

func httpOK(url string) bool {
	resp, err := directClient.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// startPostgres starts a fresh pgvector database for this run.
func startPostgres(t *testing.T) (port int, name string) {
	t.Helper()
	ensureImage(t, pgvectorImage)
	port = freePort(t)
	name = pgContainerName()
	startContainer(t, name, memPostgres, "-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"-e", "POSTGRES_USER=vornik", "-e", "POSTGRES_PASSWORD=vornik", "-e", "POSTGRES_DB=vornik",
		pgvectorImage)
	waitFor(t, "postgres", 90*time.Second, func() bool {
		return exec.Command("podman", postgresReadyArgs(name)...).Run() == nil
	})
	run(t, "podman", "exec", name, "psql", "-U", "vornik", "-d", "vornik", "-c", "CREATE EXTENSION IF NOT EXISTS vector")
	return port, name
}

// startLlama serves the pinned model with the 64K window Hermes requires.
// laneMaxTokens is the per-reply bound for llama.cpp's -n and for the
// ToolRecorder in front of it (lane design, "Amendment 2026-10-14 — a bound
// on one model reply"); a bad override fails the run.
func laneMaxTokens(t *testing.T) int {
	t.Helper()
	n, err := llamaMaxTokens(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func startLlama(t *testing.T, m laneModel, modelDir string) string {
	t.Helper()
	ensureImage(t, llamaImage)
	port := freePort(t)
	threads, err := llamaThreads(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	args := llamaServerArgs(port, modelDir, llamaImage, m, threads, laneMaxTokens(t))
	name := llamaContainerName()
	startContainer(t, name, m.memLimit(), args...)
	waitFor(t, "llama.cpp", 3*time.Minute, func() bool {
		return exec.Command("podman", "exec", name, "curl", "-sf", "localhost:8080/health").Run() == nil
	})
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// serveLoopback serves h on 127.0.0.1. Everything in the lane talks over
// loopback: Hermes runs with host networking because this host's firewall
// drops container-to-host traffic on new podman networks (bring-up
// 2026-09-30), and loopback keeps the stubs off the LAN.
func serveLoopback(t *testing.T, h http.Handler) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	srv.Start()
	t.Cleanup(srv.Close)
	return l.Addr().(*net.TCPAddr).Port
}

// buildBinaries builds the Community daemon and vornikctl from the tree.
func buildBinaries(t *testing.T, dir string) (daemon, ctl string) {
	t.Helper()
	daemon, ctl = filepath.Join(dir, "vornik"), filepath.Join(dir, "vornikctl")
	root := repoRoot(t)
	for bin, pkg := range map[string]string{daemon: "./cmd/vornik", ctl: "./cmd/vornikctl"} {
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		cmd.Dir = root
		cmd.Env = testenv.Clean()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	return daemon, ctl
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "sk-vornik-e2e-" + hex.EncodeToString(b)
}

// writeFile writes content, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, to, string(b))
}

// grantKey mints a companion key with the real vornikctl.
func grantKey(t *testing.T, ctl, apiURL, adminKey string, args ...string) string {
	t.Helper()
	_, secret := grantKeyWithID(t, ctl, apiURL, adminKey, args...)
	return secret
}

// grantKeyWithID is grantKey that also returns the key's id, which the
// daemon's companion audit rows carry (task_id "companion:<id>").
func grantKeyWithID(t *testing.T, ctl, apiURL, adminKey string, args ...string) (id, secret string) {
	t.Helper()
	cmd := exec.Command(ctl, append([]string{"companion", "grant", "--json"}, args...)...)
	cmd.Env = testenv.Clean("VORNIK_API_URL="+apiURL, "VORNIK_API_KEY="+adminKey)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("vornikctl companion grant %v: %v\n%s", args, err, stderr.String())
	}
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Secret == "" || out.ID == "" {
		t.Fatalf("grant output has no id or secret: %v\n%s", err, stdout.String())
	}
	return out.ID, out.Secret
}

// mcpCall is a direct companion MCP tools/call (used by the model-free
// scenarios H3b and H6a, and by the Vornik-side smoke).
func mcpCall(t *testing.T, apiURL, key, tool string, args map[string]any) (string, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, apiURL+"/api/v1/mcp/companion", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := directClient.Do(req)
	if err != nil {
		t.Fatalf("companion %s: %v", tool, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		t.Fatalf("companion %s: undecodable reply %q", tool, tail(string(raw), 400))
	}
	if rpc.Error != nil {
		return rpc.Error.Message, true
	}
	var text strings.Builder
	for _, c := range rpc.Result.Content {
		text.WriteString(c.Text)
	}
	return text.String(), rpc.Result.IsError
}

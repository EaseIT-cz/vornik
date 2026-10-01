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

// laneModel is one pinned model for Hermes. Hermes refuses a window below
// 64K, so each is served at 65536 tokens; models trained for 32K get YaRN.
// Only the default is the gate; the others are kept for the measurements
// recorded in the lane design's As built section.
type laneModel struct {
	Name   string // the model name Hermes is configured with
	URL    string
	File   string
	SHA256 string
	Args   []string // extra llama-server arguments
}

var yarn64K = []string{"--rope-scaling", "yarn", "--rope-scale", "2", "--yarn-orig-ctx", "32768"}

// qwen3Args: thinking off; on CPU its reasoning tokens cost minutes per
// turn. (A q8_0 KV cache with flash attention cut memory but slowed prompt
// processing from 21 to 4 tokens/s on the bring-up host, 2026-09-30.)
var qwen3Args = append([]string{"--chat-template-kwargs", `{"enable_thinking":false}`}, yarn64K...)

// laneModels are the candidates measured at bring-up (lane design, As
// built). VORNIK_E2E_HERMES_LLM picks one; defaultLaneModel is the gate.
var laneModels = map[string]laneModel{
	"qwen2.5-3b": {
		Name:   "qwen2.5-3b-instruct",
		URL:    "https://huggingface.co/Qwen/Qwen2.5-3B-Instruct-GGUF/resolve/main/qwen2.5-3b-instruct-q4_k_m.gguf",
		File:   "qwen2.5-3b-instruct-q4_k_m.gguf",
		SHA256: "626b4a6678b86442240e33df819e00132d3ba7dddfe1cdc4fbb18e0a9615c62d",
		Args:   yarn64K,
	},
	"qwen3-4b": {
		Name:   "qwen3-4b",
		URL:    "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf",
		File:   "Qwen3-4B-Q4_K_M.gguf",
		SHA256: "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
		Args:   qwen3Args,
	},
	// The gate (operator's choice, 2026-09-30): a mixture of experts with
	// about 3.6B active parameters, so it generates at small-model speed on
	// CPU. Native 128K window; reasoning effort low keeps turns short.
	"gpt-oss-20b": {
		Name:   "gpt-oss-20b",
		URL:    "https://huggingface.co/ggml-org/gpt-oss-20b-GGUF/resolve/main/gpt-oss-20b-MXFP4.gguf",
		File:   "gpt-oss-20b-MXFP4.gguf",
		SHA256: "27cd6c432c7672cb812a92f611cf3ba7bbc35928262bb1e1253ff4ee6ae35901",
		Args:   []string{"--chat-template-kwargs", `{"reasoning_effort":"low"}`},
	},
}

const defaultLaneModel = "gpt-oss-20b"

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

// cleanEnv is the ONLY environment the lane gives the processes it starts
// (the daemon, vornikctl, go build). An operator's shell can carry
// production VORNIK_* settings and secrets — this host's does — and a test
// daemon that inherits VORNIK_CONFIGS_DIR loads the production projects:
// their mail channels, MCP servers and autonomy loops (bring-up incident
// 2026-09-30). So nothing is inherited except what building and running
// need, and every VORNIK_* value is set by the lane itself.
func cleanEnv(extra ...string) []string {
	env := []string{"NO_PROXY=*", "no_proxy=*"}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "XDG_RUNTIME_DIR",
		"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "CONTAINER_HOST"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// run executes a command and fails the test with its output on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
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

// startContainer runs a detached container and removes it at cleanup.
func startContainer(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = exec.Command("podman", "rm", "-f", name).Run()
	run(t, "podman", append([]string{"run", "-d", "--name", name}, args...)...)
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
func startPostgres(t *testing.T) int {
	t.Helper()
	ensureImage(t, pgvectorImage)
	port := freePort(t)
	startContainer(t, "vornik-e2e-pg", "-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"-e", "POSTGRES_USER=vornik", "-e", "POSTGRES_PASSWORD=vornik", "-e", "POSTGRES_DB=vornik",
		pgvectorImage)
	waitFor(t, "postgres", 90*time.Second, func() bool {
		return exec.Command("podman", "exec", "vornik-e2e-pg", "pg_isready", "-U", "vornik").Run() == nil
	})
	run(t, "podman", "exec", "vornik-e2e-pg", "psql", "-U", "vornik", "-d", "vornik", "-c", "CREATE EXTENSION IF NOT EXISTS vector")
	return port
}

// startLlama serves the pinned model with the 64K window Hermes requires.
func startLlama(t *testing.T, m laneModel, modelDir string) string {
	t.Helper()
	ensureImage(t, llamaImage)
	threads := os.Getenv("VORNIK_E2E_LLAMA_THREADS")
	if threads == "" {
		threads = "6"
	}
	port := freePort(t)
	args := []string{"-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		"-v", modelDir + ":/models:ro,Z", llamaImage,
		"-m", "/models/" + m.File, "--jinja", "-c", "65536", "-np", "1",
		"-t", threads, "--temp", "0", "--seed", "1", "--host", "0.0.0.0", "--port", "8080"}
	startContainer(t, "vornik-e2e-llama", append(args, m.Args...)...)
	waitFor(t, "llama.cpp", 3*time.Minute, func() bool {
		return exec.Command("podman", "exec", "vornik-e2e-llama", "curl", "-sf", "localhost:8080/health").Run() == nil
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
		cmd.Env = cleanEnv()
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
	cmd := exec.Command(ctl, append([]string{"companion", "grant", "--json"}, args...)...)
	cmd.Env = cleanEnv("VORNIK_API_URL="+apiURL, "VORNIK_API_KEY="+adminKey)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("vornikctl companion grant %v: %v\n%s", args, err, stderr.String())
	}
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Secret == "" {
		t.Fatalf("grant output has no secret: %v\n%s", err, stdout.String())
	}
	return out.Secret
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

//go:build e2e_http

// Package spawnlaw is the end-to-end regression suite for the process-spawn law
// (https://docs.vornik.io). It boots the
// real daemon (./cmd/vornik, in both editions) and proves two things over HTTP, for every surface the
// law hardened: the capability still works, and no daemon surface can make the
// daemon run a program of its choosing.
//
// S1a (MCP): a stdio server declared in the config files and an HTTP server are
// both discovered with their tools; the UI probe lists an HTTP server's tools;
// the full onboarding path (add form -> proposal -> approve -> apply ->
// discovery) works for an HTTP server; and the probe, the add form and the raw
// project editor refuse a stdio server. Every refused stdio command points at a
// marker script that would create a file if it ever ran, and the suite asserts
// it never appears.
//
// Run: go test -tags e2e_http -count=1 ./test/e2e/spawnlaw/
// (part of `make test-e2e-http`). No podman, no PostgreSQL, no LLM.
package spawnlaw

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	httpBase     string
	configFile   string
	marker       string // created by markerScript if anything ever executes it
	markerScript string
	fakeHTTPURL  string
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[spawnlaw e2e]", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	root, err := moduleRoot()
	if err != nil {
		return 1, err
	}
	work, err := os.MkdirTemp("", "vornik-spawnlaw-e2e-*")
	if err != nil {
		return 1, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	fake := filepath.Join(work, "fakemcp")
	if err := goBuild(root, fake, "-tags", "e2e_http", "./test/e2e/spawnlaw/fakemcp"); err != nil {
		return 1, fmt.Errorf("build fakemcp: %w", err)
	}
	// ./cmd/vornik, the daemon main both editions ship: every surface this
	// suite exercises (MCP discovery, the UI probe, onboarding, the project
	// editor) is Community code. It used to build the Enterprise main with its
	// edition flag, which the CE export refuses to publish; the suite then
	// skipped in CE and proved nothing there.
	daemonBin := filepath.Join(work, "vornik")
	if err := goBuild(root, daemonBin, "./cmd/vornik"); err != nil {
		return 1, fmt.Errorf("build daemon: %w", err)
	}

	// The HTTP MCP server the daemon discovers and the UI probes.
	fakePort := freePort()
	fakeHTTPURL = fmt.Sprintf("http://127.0.0.1:%d/mcp", fakePort)
	fakeSrv := exec.Command(fake, "-http", fmt.Sprintf("127.0.0.1:%d", fakePort))
	if err := fakeSrv.Start(); err != nil {
		return 1, fmt.Errorf("start fake http mcp: %w", err)
	}
	defer func() { _ = fakeSrv.Process.Kill(); _, _ = fakeSrv.Process.Wait() }()

	// The stdio MCP server the config file declares. The MCP client only
	// launches allowlisted launchers or programs in system paths, so the fake
	// runs under python3, the way a real stdio server usually does.
	stdioFake := filepath.Join(work, "fake_mcp.py")
	if err := os.WriteFile(stdioFake, []byte(fakeMCPPython), 0o644); err != nil { //nolint:gosec // test fixture
		return 1, err
	}

	marker = filepath.Join(work, "SPAWNED")
	markerScript = filepath.Join(work, "would-run.sh")
	if err := os.WriteFile(markerScript, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		return 1, err
	}

	port := freePort()
	httpBase = fmt.Sprintf("http://127.0.0.1:%d", port)
	configsTree := filepath.Join(work, "configs")
	configFile = filepath.Join(work, "config.yaml")
	if err := layout(configsTree, work, port, stdioFake); err != nil {
		return 1, fmt.Errorf("layout: %w", err)
	}

	logPath := filepath.Join(work, "daemon.log")
	logFile, _ := os.Create(logPath)
	daemon := exec.Command(daemonBin)
	daemon.Dir = work
	daemon.Env = append(os.Environ(), "VORNIK_CONFIG="+configFile, "VORNIK_CONFIGS_DIR="+configsTree)
	daemon.Stdout, daemon.Stderr = logFile, logFile
	if err := daemon.Start(); err != nil {
		return 1, fmt.Errorf("start daemon: %w", err)
	}
	defer func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = daemon.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = daemon.Process.Kill()
			<-done
		}
		_ = logFile.Close()
	}()
	if err := waitHealthy(40 * time.Second); err != nil {
		if b, rerr := os.ReadFile(logPath); rerr == nil {
			fmt.Fprintln(os.Stderr, "[spawnlaw e2e] daemon log:\n"+string(b))
		}
		return 1, err
	}
	code := m.Run()
	if code != 0 {
		if b, rerr := os.ReadFile(logPath); rerr == nil {
			fmt.Fprintln(os.Stderr, "[spawnlaw e2e] daemon log:\n"+string(b))
		}
	}
	return code, nil
}

// --- S1a: MCP -----------------------------------------------------------------

// Discovery still works for both kinds the law allows: a stdio server whose
// program comes from the config files, and an HTTP server.
func TestMCPDiscovery_ConfigFileStdioAndHTTPServers(t *testing.T) {
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		_, body := do(t, http.MethodGet, "/api/v1/mcp/servers", "", nil)
		last = body
		if serverHasTool(body, "config-stdio", "echo") && serverHasTool(body, "config-http", "echo") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("both configured servers must be discovered with the echo tool; last listing: %s", last)
}

// The onboarding probe still works for an HTTP server.
func TestMCPProbe_HTTPServerListsItsTools(t *testing.T) {
	resp, body := postForm(t, "/ui/admin/control-plane/mcp/probe", url.Values{
		"transport": {"streamable-http"}, "url": {fakeHTTPURL},
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "echo") {
		t.Fatalf("probe of an HTTP server must list its tools: HTTP %d, %s", resp.StatusCode, body)
	}
}

// The incident: the probe passed its command field to exec.Command.
func TestMCPProbe_StdioIsRefusedAndNothingRuns(t *testing.T) {
	_, body := postForm(t, "/ui/admin/control-plane/mcp/probe", url.Values{
		"transport": {"stdio"}, "command": {markerScript},
	})
	if !strings.Contains(body, "Invalid endpoint") {
		t.Fatalf("a stdio probe must be refused, got: %s", body)
	}
	assertNothingRan(t)
}

func TestMCPAddForm_StdioIsRefusedAndNothingRuns(t *testing.T) {
	resp, _ := postForm(t, "/ui/admin/control-plane/mcp", url.Values{
		"action": {"add"}, "name": {"sneaky"}, "transport": {"stdio"}, "command": {markerScript},
	})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "mcp-stdio-refused") {
		t.Fatalf("the add form must refuse a stdio server, redirect was %q", loc)
	}
	assertNothingRan(t)
}

// The full onboarding path still works for an HTTP server: add form, proposal,
// approve, apply, reload, discovery.
func TestMCPOnboarding_HTTPServerThroughAProposal(t *testing.T) {
	resp, _ := postForm(t, "/ui/admin/control-plane/mcp", url.Values{
		"action": {"add"}, "name": {"onboarded-http"}, "transport": {"streamable-http"}, "url": {fakeHTTPURL},
	})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "mcp-proposed") {
		t.Fatalf("the add form must draft a proposal for an HTTP server, redirect was %q", loc)
	}
	id := latestProposalID(t, "onboarded-http")
	if r, b := doJSON(t, http.MethodPost, "/api/v1/operator/proposals/"+id+"/decide", map[string]any{"decision": "approve", "actor": "e2e"}); r.StatusCode >= 300 {
		t.Fatalf("approve: HTTP %d %s", r.StatusCode, b)
	}
	if r, b := doJSON(t, http.MethodPost, "/api/v1/operator/proposals/"+id+"/apply", map[string]any{"actor": "e2e", "ackDaemon": true}); r.StatusCode >= 300 {
		t.Fatalf("apply: HTTP %d %s", r.StatusCode, b)
	}
	cfg, _ := os.ReadFile(configFile)
	if !strings.Contains(string(cfg), "onboarded-http") {
		t.Fatalf("the applied proposal must add the server to the config file:\n%s", cfg)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, body := do(t, http.MethodGet, "/api/v1/mcp/servers", "", nil); serverHasTool(body, "onboarded-http", "echo") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("the onboarded HTTP server must be discovered with its tool after apply")
}

// The raw project editor writes files directly; it must not add a stdio server.
func TestProjectConfigEditor_StdioIsRefusedAndNothingRuns(t *testing.T) {
	path := filepath.Join(filepath.Dir(configFile), "configs", "projects", "p1.yaml")
	before, _ := os.ReadFile(path)
	content := string(before) + fmt.Sprintf("mcp:\n  servers:\n    - name: sneaky\n      transport: stdio\n      command: %s\n", markerScript)
	resp, body := postForm(t, "/ui/projects/p1/config", url.Values{"content": {content}})
	if resp.StatusCode < 400 {
		t.Fatalf("saving a stdio server through the editor must fail, got HTTP %d: %s", resp.StatusCode, body)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused save must leave the project file untouched")
	}
	assertNothingRan(t)
}

func assertNothingRan(t *testing.T) {
	t.Helper()
	time.Sleep(2 * time.Second) // give anything that was wrongly launched time to run
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused stdio command RAN on the daemon host")
	}
}

// --- helpers -------------------------------------------------------------------

func serverHasTool(listing, server, tool string) bool {
	var v any
	if json.Unmarshal([]byte(listing), &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(n any) bool {
		switch x := n.(type) {
		case map[string]any:
			if fmt.Sprint(x["name"]) == server {
				b, _ := json.Marshal(x)
				return strings.Contains(string(b), `"`+tool+`"`)
			}
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

func latestProposalID(t *testing.T, mention string) string {
	t.Helper()
	_, body := do(t, http.MethodGet, "/api/v1/operator/proposals", "", nil)
	var v any
	_ = json.Unmarshal([]byte(body), &v)
	var id string
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			b, _ := json.Marshal(x)
			if pid, ok := x["id"].(string); ok && strings.Contains(string(b), mention) {
				id = pid
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	if id == "" {
		t.Fatalf("no proposal mentioning %q in %s", mention, body)
	}
	return id
}

func postForm(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	return do(t, http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

func doJSON(t *testing.T, method, path string, v any) (*http.Response, string) {
	t.Helper()
	b, _ := json.Marshal(v)
	return do(t, method, path, "application/json", strings.NewReader(string(b)))
}

func do(t *testing.T, method, path, contentType string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, httpBase+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(httpBase + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("daemon not healthy within %s", timeout)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func goBuild(root, out string, args ...string) error {
	cmd := exec.Command("go", append(append([]string{"build", "-o", out}, args[:len(args)-1]...), args[len(args)-1])...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

func layout(configsTree, work string, port int, stdioFake string) error {
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configsTree, sub), 0o755); err != nil {
			return err
		}
	}
	files := map[string]string{
		configFile: fmt.Sprintf(daemonConfig, port, filepath.Join(work, "vornik.db"),
			filepath.Join(work, "artifacts"), filepath.Join(work, "artifacts"), stdioFake, fakeHTTPURL),
		filepath.Join(configsTree, "projects", "p1.yaml"):   projectYAML,
		filepath.Join(configsTree, "swarms", "p1-swarm.md"): swarmMD,
		filepath.Join(configsTree, "workflows", "p1-wf.md"): workflowMD,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // test fixtures
			return err
		}
	}
	return nil
}

// daemonConfig: SQLite, loopback, auth off, no LLM, and the two MCP servers the
// law allows: a stdio server from the config file and an HTTP server.
// Args: port, db, artifacts, artifacts, the stdio fake's script, fake HTTP URL.
const daemonConfig = `server:
  address: "127.0.0.1:%d"
database:
  driver: sqlite
  path: "%s"
storage:
  artifacts_path: "%s"
artifacts:
  artifacts_path: "%s"
scheduler:
  max_concurrent_tasks: 1
logging:
  level: info
  format: text
api:
  auth_enabled: false
metrics:
  enabled: false
tracing:
  enabled: false
chat:
  enabled: false
memory:
  enabled: false
telegram:
  enabled: false
runtime:
  userns_mode: ""
  run_as_user: ""
mcp:
  servers:
    - name: config-stdio
      transport: stdio
      command: python3
      args: ["%s"]
    - name: config-http
      transport: streamable-http
      url: "%s"
`

const projectYAML = `projectId: "p1"
displayName: "Spawn law e2e"
swarmId: "p1-swarm"
defaultWorkflowId: "p1-wf"
defaultPriority: 30
maxConcurrentTasks: 1
permissions:
  secrets: []
  allowedTools:
    - "current_time"
`

const swarmMD = `---
swarmId: "p1-swarm"
displayName: "p1 swarm"
leadRole: "lead"
roles:
  - name: "lead"
    description: "Lead."
    runtime:
      image: "vornik-agent:latest"
    permissions:
      allowedTools:
        - "current_time"
---

# p1 swarm

## Role prompts

### lead

You are the lead.
`

const workflowMD = `---
workflowId: "p1-wf"
displayName: "p1 workflow"
entrypoint: "run"
maxStepVisits: 1
steps:
  run:
    type: "agent"
    role: "lead"
    on_success: "done"
    on_fail: "failed"
    timeout: "5m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "failed"
---

# p1 workflow

## Prompts

### run

Do the thing.
`

// fakeMCPPython is the stdio MCP server: initialize and tools/list, one tool.
const fakeMCPPython = `import json, sys
for line in sys.stdin:
    try:
        req = json.loads(line)
    except ValueError:
        continue
    if "id" not in req:
        continue
    if req.get("method") == "initialize":
        result = {"protocolVersion": "2025-03-26", "capabilities": {"tools": {}},
                  "serverInfo": {"name": "fake-stdio", "version": "1"}}
    elif req.get("method") == "tools/list":
        result = {"tools": [{"name": "echo", "description": "Echo the input back.",
                             "inputSchema": {"type": "object"}}]}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": req["id"],
                          "error": {"code": -32601, "message": "method not found"}}), flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": result}), flush=True)
`

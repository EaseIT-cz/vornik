//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// stack is one run's Vornik side plus the Hermes model.
type stack struct {
	dir       string
	apiURL    string // for the test process and vornikctl
	hermesURL string // the daemon as Hermes sees it (loopback; host networking)
	adminKey  string
	brokerKey string
	memoryKey string
	llm       *LLMStub
	mailRead  *MCPStub
	mailSend  *MCPStub
	llamaURL  string
	// hermesModel is set in stub mode (VORNIK_E2E_HERMES_MODEL=stub): a
	// scripted model for Hermes instead of llama.cpp (lane design §5).
	hermesModel     *LLMStub
	hermesRec       *ToolRecorder // real-model mode: between Hermes and llama.cpp
	hermesModelName string        // the name Hermes's config gives the model
	// replyTask and replyAction are H4's mail-reply task and its action.
	replyTask, replyAction string
	daemonLog              string
	hermesHome             string
	// Agent-administered Vornik's DoD lane (plan P8): the binaries and the
	// config path, the bank stub and the agent's mail server.
	ctl, cfgPath string
	pgPort       int
	bank         *BankStub
	agentMail    *MCPStub
	bankURL      string
	agentMailURL string
	// cfgEdit, when set, rewrites config.yaml before the daemon starts (an
	// arm that needs other chat or agent_admin settings, design §18.6 item
	// 2); daemon, daemonCmd and daemonLogf let restartDaemon replace it.
	cfgEdit   func(string) string
	cfgFiles  map[string]string // extra files under configs/, written before boot
	daemon    string
	daemonCmd *exec.Cmd
	daemonOut *os.File
}

// The stub mailbox: one invoice message whose body carries the sentinel.
func mailReadStub() *MCPStub {
	return NewMCPStub("google-workspace",
		MCPTool{Name: "time_getCurrentDate", Handle: func(json.RawMessage) (string, bool) {
			return `{"date":"2026-09-30T09:00:00Z"}`, false
		}},
		MCPTool{Name: "gmail_search", Handle: func(json.RawMessage) (string, bool) {
			return `{"messages":[{"id":"` + invoiceMessageID + `","from":"billing@acme.example","subject":"Invoice 4411","received_at":"2026-09-29T15:04:00Z"}]}`, false
		}},
		MCPTool{Name: "gmail_get", Handle: func(json.RawMessage) (string, bool) {
			return `{"id":"` + invoiceMessageID + `","from":"billing@acme.example","subject":"Invoice 4411",` +
				`"received_at":"2026-09-29T15:04:00Z","body":"Please pay invoice 4411 by Friday. Internal ref ` + rawMailSentinel + `."}`, false
		}},
	)
}

func mailSendStub() *MCPStub {
	return NewMCPStub("gmail-send", MCPTool{Name: "gmail_send", Handle: func(json.RawMessage) (string, bool) {
		return `{"sent":true,"id":"sent-1"}`, false
	}})
}

// Scripted broker-agent plans (lane design §3.2).
const (
	digestMarker = "Build a digest of the mail"
	replyMarker  = "Draft a reply to one message"
	digestLine   = "Invoice 4411 from acme.example is due Friday"
	replyBody    = "Thank you, we will pay invoice 4411 on Friday."
)

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func digestScript(digest map[string]any) Script {
	return Script{Marker: digestMarker, Final: "Digest written.", Steps: []ScriptStep{
		{ToolSuffix: "gmail_search", Args: map[string]any{"query": "newer_than:1d"}},
		{ToolSuffix: "gmail_get", Args: map[string]any{"id": invoiceMessageID}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/digest.json", "content": mustJSON(digest)}},
	}}
}

func goodDigest() map[string]any {
	return map[string]any{
		"items": []map[string]any{{"from_domain": "acme.example", "received_at": "2026-09-29T15:04:00Z",
			"category": "invoice", "needs_reply": true, "one_line": digestLine}},
		"counts": map[string]any{"total": 1, "shown": 1, "suppressed": 0},
	}
}

func replyScript() Script {
	proposal := map[string]any{"action": "send_reply", "args": map[string]any{
		"to": "billing@acme.example", "subject": "Re: Invoice 4411", "in_reply_to": invoiceMessageID, "body": replyBody}}
	summary := map[string]any{"found": true, "drafted": true, "one_line": "Confirms payment of invoice 4411 on Friday."}
	return Script{Marker: replyMarker, Final: "Reply drafted.", Steps: []ScriptStep{
		{ToolSuffix: "gmail_search", Args: map[string]any{"query": "from:acme.example"}},
		{ToolSuffix: "gmail_get", Args: map[string]any{"id": invoiceMessageID}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/reply-proposal.json", "content": mustJSON(proposal)}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/reply-summary.json", "content": mustJSON(summary)}},
	}}
}

// agentImage is the image agent-defined roles run (agent_admin.agent_image):
// the lane's override when set, else the shipped default.
func agentImage() string {
	if img := os.Getenv("VORNIK_E2E_AGENT_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/easeit-cz/vornik-agent:latest"
}

// writeConfigs lays out config.yaml and configs/ for the run: the shipped
// broker workflows and swarm, the example broker project pointed at the
// stubs, and a memory project.
func writeConfigs(t *testing.T, s *stack, pgPort, apiPort int, llmURL, readURL, sendURL string) string {
	t.Helper()
	root := repoRoot(t)
	cfgDir := filepath.Join(s.dir, "etc")
	for _, wf := range []string{"mail-digest.md", "mail-reply.md"} {
		copyFile(t, filepath.Join(root, "configs", "workflows", wf), filepath.Join(cfgDir, "configs", "workflows", wf))
	}
	// The agent admin verbs render from the shipped templates, which a
	// deployment installs with make install-config-assets (plan P8).
	// The tree descends: the recipes catalogue is a subtree (design §19).
	src := filepath.Join(root, "configs", "agent-templates")
	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		copyFile(t, path, filepath.Join(cfgDir, "configs", "agent-templates", rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	swarm, err := os.ReadFile(filepath.Join(root, "configs", "swarms", "broker-swarm.md"))
	if err != nil {
		t.Fatal(err)
	}
	if img := os.Getenv("VORNIK_E2E_AGENT_IMAGE"); img != "" {
		swarm = []byte(strings.ReplaceAll(string(swarm), "ghcr.io/easeit-cz/vornik-agent:latest", img))
	}
	swarm = []byte(strings.ReplaceAll(string(swarm), `model: "zai.glm-5"`, `model: "`+scriptedModel+`"`))
	writeFile(t, filepath.Join(cfgDir, "configs", "swarms", "broker-swarm.md"), string(swarm))

	// The shipped example, with its two MCP servers pointed at the stubs.
	writeFile(t, filepath.Join(cfgDir, "configs", "projects", "broker-mail.yaml"), fmt.Sprintf(`projectId: "broker-mail"
displayName: "Mail broker (e2e)"
swarmId: "broker-swarm"
defaultWorkflowId: "mail-digest"
defaultPriority: 50
maxConcurrentTasks: 2
broker: true
mcp:
  servers:
    - name: "google-workspace"
      transport: "streamable-http"
      url: %q
      broker_read_only: true
      allowed_tools: ["time_getCurrentDate", "gmail_search", "gmail_get"]
    - name: "gmail-send"
      transport: "streamable-http"
      url: %q
      broker_write: true
      allowed_tools: ["gmail_send"]
permissions:
  allowedTools:
    - "current_time"
    - "file_write"
    - "mcp__google-workspace__time_getCurrentDate"
    - "mcp__google-workspace__gmail_search"
    - "mcp__google-workspace__gmail_get"
`, readURL, sendURL))
	writeFile(t, filepath.Join(cfgDir, "configs", "projects", "assistant-memory.yaml"), `projectId: "assistant-memory"
displayName: "Hermes memory (e2e)"
swarmId: "broker-swarm"
defaultWorkflowId: "mail-digest"
defaultPriority: 50
`)
	cfg := fmt.Sprintf(`server:
  address: "127.0.0.1:%d"
  unix_socket: %q
database:
  driver: postgres
  host: 127.0.0.1
  port: %d
  name: vornik
  user: vornik
  password: vornik
  sslmode: disable
storage:
  artifacts_path: %q
artifacts:
  artifacts_path: %q
scheduler:
  max_concurrent_tasks: 2
  lease_timeout: 5m
logging:
  # debug: successful requests are logged only at debug, and H1 checks
  # that the plugin read /api/v1/capabilities.
  level: debug
  format: text
api:
  auth_enabled: true
  api_keys: [%q]
admin:
  enabled: true
  allowed_keys: [%q]
metrics:
  enabled: false
tracing:
  enabled: false
chat:
  enabled: true
  provider: http
  endpoint: %q
  api_key: "stub"
  model: %q
memory:
  enabled: true
  embedding_endpoint: %q
  embedding_model: "e2e-embed"
  embedding_dimension: %d
telegram:
  enabled: false
broker:
  writes: "on"
agent_admin:
  agent_image: %q
runtime:
  # Rootless podman: the agent image is built for the host UID, so the
  # container keeps it (as production does) to read its mounted contract.
  userns_mode: "keep-id"
  default_network: "daemon-only"
  project_workspace_path: %q
  agent_llm:
    model: %q
mcp:
  servers: []
`, apiPort, filepath.Join(s.dir, "vornik.sock"), pgPort,
		filepath.Join(s.dir, "artifacts"), filepath.Join(s.dir, "artifacts"),
		s.adminKey, s.adminKey, llmURL, scriptedModel, llmURL, embeddingDim, agentImage(),
		filepath.Join(s.dir, "workspaces"), scriptedModel)
	if s.cfgEdit != nil {
		cfg = s.cfgEdit(cfg)
	}
	for rel, content := range s.cfgFiles {
		writeFile(t, filepath.Join(cfgDir, "configs", rel), content)
	}
	path := filepath.Join(cfgDir, "config.yaml")
	writeFile(t, path, cfg)
	return path
}

// startStack brings the Vornik side up and mints the two Hermes keys.
func startStack(t *testing.T) *stack {
	t.Helper()
	return startStackWith(t, nil, nil)
}

// startStackWith is startStack with config.yaml rewritten by edit and files
// (paths under configs/) written first.
func startStackWith(t *testing.T, edit func(string) string, files map[string]string) *stack {
	t.Helper()
	requirePodman(t)
	s := &stack{dir: t.TempDir(), adminKey: randomKey(t), cfgEdit: edit, cfgFiles: files}
	reapStaleContainers()
	pgPort, _ := startPostgres(t)
	s.pgPort = pgPort

	s.llm = &LLMStub{EmbeddingDim: embeddingDim, Final: "OK", Scripts: []Script{digestScript(goodDigest()), replyScript()}}
	llmPort := serveLoopback(t, s.llm)
	s.mailRead, s.mailSend = mailReadStub(), mailSendStub()
	readPort := serveLoopback(t, s.mailRead)
	sendPort := serveLoopback(t, s.mailSend)

	s.bank = NewBankStub()
	s.bankURL = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, s.bank))
	s.agentMail = agentMailStub()
	s.agentMailURL = fmt.Sprintf("http://127.0.0.1:%d/mcp", serveLoopback(t, s.agentMail))

	apiPort := freePort(t)
	s.apiURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	s.hermesURL = s.apiURL // Hermes runs with host networking
	cfgPath := writeConfigs(t, s, pgPort, apiPort,
		fmt.Sprintf("http://127.0.0.1:%d/v1", llmPort),
		fmt.Sprintf("http://127.0.0.1:%d/mcp", readPort),
		fmt.Sprintf("http://127.0.0.1:%d/mcp", sendPort))

	daemon, ctl := buildBinaries(t, s.dir)
	s.ctl, s.cfgPath, s.daemon = ctl, cfgPath, daemon
	s.daemonLog = filepath.Join(s.dir, "daemon.log")
	logf, err := os.Create(s.daemonLog)
	if err != nil {
		t.Fatal(err)
	}
	s.daemonOut = logf
	s.startDaemon(t)
	t.Cleanup(func() {
		s.stopDaemon()
		_ = logf.Close()
		if keep := os.Getenv("VORNIK_E2E_KEEP"); keep != "" {
			_ = os.MkdirAll(keep, 0o755)
			copyFile(t, s.daemonLog, filepath.Join(keep, "daemon.log"))
			if s.hermesHome != "" {
				_ = exec.Command("cp", "-r", s.hermesHome, filepath.Join(keep, "hermes-home")).Run()
			}
			t.Logf("kept the daemon log and Hermes home in %s", keep)
		}
		if t.Failed() {
			b, _ := os.ReadFile(s.daemonLog)
			t.Logf("--- daemon log (tail) ---\n%s", tail(string(b), 6000))
		}
	})
	waitFor(t, "the vornik daemon", 2*time.Minute, func() bool { return httpOK(s.apiURL + "/health") })
	// Guard: the daemon must have loaded exactly the lane's projects. Any
	// other project means it reached a real configuration; stop at once.
	if got := listProjects(t, s.apiURL, s.adminKey); strings.Join(got, ",") != "assistant-memory,broker-mail" {
		_ = s.daemonCmd.Process.Kill()
		t.Fatalf("the test daemon loaded projects %v, not only the lane's own: refusing to continue", got)
	}

	s.brokerKey = grantKey(t, ctl, s.apiURL, s.adminKey, "--project", "broker-mail", "--client", "hermes",
		"--workflows", "mail-digest,mail-reply", "--budget-usd", "5")
	s.memoryKey = grantKey(t, ctl, s.apiURL, s.adminKey, "--project", "assistant-memory", "--client", "hermes",
		"--memory-all", "--no-delegate")
	return s
}

// startDaemon starts the lane daemon with the allowlisted environment.
func (s *stack) startDaemon(t *testing.T) {
	t.Helper()
	cmd := exec.Command(s.daemon)
	cmd.Env = cleanEnv("VORNIK_CONFIG="+s.cfgPath, "VORNIK_CONFIGS_DIR="+filepath.Join(filepath.Dir(s.cfgPath), "configs"),
		"VORNIK_DATA_DIR="+filepath.Join(s.dir, "data"))
	cmd.Stdout, cmd.Stderr = s.daemonOut, s.daemonOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.daemonCmd = cmd
}

// stopDaemon interrupts the running daemon and waits for it.
func (s *stack) stopDaemon() {
	cmd := s.daemonCmd
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	s.daemonCmd = nil
}

// restartDaemon stops the daemon, rewrites config.yaml with edit, and starts
// it again on the same database and tree, as an operator's config edit and
// restart do.
func (s *stack) restartDaemon(t *testing.T, edit func(string) string) {
	t.Helper()
	s.stopDaemon()
	raw, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.cfgPath, edit(string(raw)))
	s.startDaemon(t)
	waitFor(t, "the restarted vornik daemon", 2*time.Minute, func() bool { return httpOK(s.apiURL + "/health") })
}

// listProjects returns the daemon's project ids, sorted.
func listProjects(t *testing.T, apiURL, adminKey string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, apiURL+"/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := directClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Projects []struct {
			ProjectID string `json:"projectId"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET /api/v1/projects: %s", tail(string(raw), 400))
	}
	ids := make([]string, 0, len(out.Projects))
	for _, p := range out.Projects {
		ids = append(ids, p.ProjectID)
	}
	sort.Strings(ids)
	return ids
}

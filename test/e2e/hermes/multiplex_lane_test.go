//go:build e2e_hermes

package hermes

// The multiplexed-profile arm (lane design, "Amendment 2026-10-14 — the
// multiplexed-profile arm (O2)"; design 24 O2). One Hermes gateway, started
// the supported way (gateway.multiplex_profiles: true, `gateway run`), serves
// three profiles through its API server's /p/<profile>/ routes: default and
// beta, each with its own Vornik URL and keys for its own projects, and gamma
// with none. Found at: code review upstream (hermes-agent#137179, catalogue
// rule 11); a user observes it in one gateway serving several profiles. The
// 0.21.4 plugin read os.environ at register(), so every profile used the
// launch profile's keys; VORNIK_E2E_HERMES_PLUGIN_DIR points this arm at that
// plugin to show it red.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	muxMarker    = "Which Vornik workflows can this profile run"
	muxPrompt    = muxMarker + "? Use your Vornik tools, and recall what you know about my dentist."
	pluginDirEnv = "VORNIK_E2E_HERMES_PLUGIN_DIR"
)

var muxProfiles = []string{"default", "beta", "gamma"}

// muxProjectFiles are the second broker project and the second memory
// project beta's keys belong to.
func muxProjectFiles() map[string]string {
	return map[string]string{
		"projects/broker-b.yaml": `projectId: "broker-b"
displayName: "Mail broker B (e2e multiplex)"
swarmId: "broker-swarm"
defaultWorkflowId: "mail-digest"
defaultPriority: 50
maxConcurrentTasks: 2
broker: true
mcp:
  servers:
    - name: "google-workspace"
      transport: "streamable-http"
      url: "{{MAIL_READ_URL}}"
      broker_read_only: true
      allowed_tools: ["time_getCurrentDate", "gmail_search", "gmail_get"]
    - name: "gmail-send"
      transport: "streamable-http"
      url: "{{MAIL_SEND_URL}}"
      broker_write: true
      allowed_tools: ["gmail_send"]
permissions:
  allowedTools:
    - "current_time"
    - "file_write"
`,
		"projects/memory-b.yaml": `projectId: "memory-b"
displayName: "Hermes memory B (e2e multiplex)"
swarmId: "broker-swarm"
defaultWorkflowId: "mail-digest"
defaultPriority: 50
`,
	}
}

// muxTurnTag marks a profile's prompt, so its model endpoint can be shown to
// have received that profile's turn.
func muxTurnTag(profile string) string { return "[turn for profile " + profile + "]" }

// pluginSource is the plugin directory the arm installs: the tree's, or
// VORNIK_E2E_HERMES_PLUGIN_DIR (to run the arm against another version).
func pluginSource(t *testing.T) string {
	t.Helper()
	if d := os.Getenv(pluginDirEnv); d != "" {
		return d
	}
	return filepath.Join(repoRoot(t), "contrib", "hermes-companion")
}

func copyPlugin(t *testing.T, src, home string) {
	t.Helper()
	dst := filepath.Join(home, "plugins", "vornik-companion")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "tests" || e.Name() == "__pycache__" {
			continue
		}
		run(t, "cp", "-r", filepath.Join(src, e.Name()), dst+"/")
	}
}

// muxProfileConfig is one profile's config.yaml: the lane's Hermes settings
// with the profile's own model endpoint; the default profile also turns on
// multiplexing.
func muxProfileConfig(model, baseURL string, launch bool) string {
	cfg := fmt.Sprintf(`model:
  default: %q
  provider: "custom"
  base_url: %q
  context_length: 65536
plugins:
  enabled: ["vornik-companion"]
  disabled: []
memory:
  provider: "vornik-companion"
tools:
  tool_search:
    enabled: "off"
platform_toolsets:
  cli: ["vornik", "memory"]
  api_server: ["vornik", "memory"]
`, model, baseURL)
	if launch {
		cfg += "gateway:\n  multiplex_profiles: true\n"
	}
	return cfg
}

// readMuxAuditRows reads the daemon's companion audit rows once.
func readMuxAuditRows() ([]auditRow, error) {
	out, err := exec.Command("podman", "exec", pgContainerName(), "psql", "-U", "vornik", "-d", "vornik", "-tA", "-F", auditFieldSep, "-c",
		"SELECT id, task_id, project_id, tool_name FROM tool_audit_log WHERE task_id LIKE 'companion:%' ORDER BY created_at").Output()
	if err != nil {
		return nil, fmt.Errorf("read companion audit rows: %w", err)
	}
	return parseAuditRows(string(out))
}

// muxAuditRows is one window boundary: the companion audit rows once their
// count has held still for a (speed-scaled) quiet window (review 849e F1).
func muxAuditRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := settleAuditRows(readMuxAuditRows, slow(2*time.Second), slow(time.Minute), time.Now, time.Sleep)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// muxTurn is one OpenAI-style turn on a profile's route of the gateway's API
// server, with that profile's API_SERVER_KEY.
func muxTurn(t *testing.T, base, profile, key, model string) string {
	t.Helper()
	url := base + "/v1/chat/completions"
	if profile != "default" {
		url = base + "/p/" + profile + "/v1/chat/completions"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": muxPrompt + " " + muxTurnTag(profile)}}})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: slow(20 * time.Minute), Transport: &http.Transport{Proxy: nil}}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s turn: %v", profile, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s turn: HTTP %d: %s", profile, resp.StatusCode, tail(string(raw), 1500))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(raw, &out)
	reply := ""
	if len(out.Choices) > 0 {
		reply = out.Choices[0].Message.Content
	}
	t.Logf("%s turn: %s to %s (%s), reply %q", profile, start.UTC().Format("15:04:05.000"), time.Now().UTC().Format("15:04:05.000"),
		time.Since(start).Round(time.Millisecond), tail(reply, 300))
	return reply
}

func TestHermesMultiplexedProfiles(t *testing.T) {
	start := time.Now()
	ensureHermesImage(t)
	s := startStackWith(t, nil, muxProjectFiles())
	grant := func(profile, role, project string, args ...string) (laneKey, string) {
		id, secret := grantKeyWithID(t, s.ctl, s.apiURL, s.adminKey, append([]string{"--project", project, "--client", "hermes"}, args...)...)
		return laneKey{Profile: profile, Role: role, ID: id, Project: project}, secret
	}
	broker := []string{"--workflows", "mail-digest,mail-reply", "--budget-usd", "5"}
	memory := []string{"--memory-all", "--no-delegate"}
	kA, aBroker := grant("default", "broker", "broker-mail", broker...)
	kAm, aMemory := grant("default", "memory", "assistant-memory", memory...)
	kB, bBroker := grant("beta", "broker", "broker-b", broker...)
	kBm, bMemory := grant("beta", "memory", "memory-b", memory...)
	keys := []laneKey{kA, kAm, kB, kBm}

	// Each profile's model endpoint, so what each was offered is its own.
	models := map[string]toolLister{}
	baseURLs := map[string]string{}
	stubs := map[string]*LLMStub{}
	var modelName string
	if os.Getenv("VORNIK_E2E_HERMES_MODEL") == "stub" {
		modelName = scriptedModel
		for _, p := range muxProfiles {
			st := &LLMStub{Final: "Done.", Scripts: []Script{{Marker: muxMarker, Final: "Done (" + p + ").", Steps: []ScriptStep{
				{ToolSuffix: "vornik_catalog", Args: map[string]any{}},
				{ToolSuffix: "vornik_recall", Args: map[string]any{"query": "dentist"}},
			}}}}
			stubs[p], models[p] = st, st
			baseURLs[p] = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, st))
		}
	} else {
		m := pickModel(t)
		requireMemory(t, "before llama.cpp", llamaStageItems(m)...)
		modelName = m.Name
		upstream := strings.TrimSuffix(startLlama(t, m, ensureModel(t, m)), "/v1")
		for _, p := range muxProfiles {
			rec := NewToolRecorder(upstream, laneMaxTokens(t))
			models[p] = rec
			baseURLs[p] = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, rec))
		}
	}

	// The Hermes home: default written here; named profiles created and
	// their settings written with Hermes's own commands, as a user would.
	home := filepath.Join(s.dir, "hermes-mux-home")
	s.hermesHome = home
	t.Cleanup(func() { _ = exec.Command("podman", "unshare", "chown", "-R", "0:0", home).Run() })
	src := pluginSource(t)
	manifest, err := os.ReadFile(filepath.Join(src, "plugin.yaml"))
	if err != nil {
		t.Fatalf("plugin under test has no plugin.yaml: %v", err)
	}
	version := ""
	for _, line := range strings.Split(string(manifest), "\n") {
		if strings.HasPrefix(line, "version:") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "version:"))
		}
	}
	t.Logf("plugin under test: %s (version %s)", src, version)
	copyPlugin(t, src, home)
	writeFile(t, filepath.Join(home, "config.yaml"), muxProfileConfig(modelName, baseURLs["default"], true))
	apiPort := freePort(t)
	apiKeys := map[string]string{}
	for _, p := range muxProfiles {
		apiKeys[p] = randomKey(t)
	}
	writeFile(t, filepath.Join(home, ".env"), fmt.Sprintf(
		"OPENAI_API_KEY=unused\nAPI_SERVER_ENABLED=true\nAPI_SERVER_HOST=127.0.0.1\nAPI_SERVER_PORT=%d\nAPI_SERVER_KEY=%s\n", apiPort, apiKeys["default"]))
	set := func(profile, name, value string) string {
		if profile == "default" {
			return fmt.Sprintf("hermes config set %s %s", name, value)
		}
		return fmt.Sprintf("hermes -p %s config set %s %s", profile, name, value)
	}
	script := strings.Join([]string{
		"set -e",
		"hermes profile create beta", "hermes profile create gamma",
		set("default", "VORNIK_URL", s.hermesURL), set("default", "VORNIK_BROKER_TOKEN", aBroker), set("default", "VORNIK_MEMORY_TOKEN", aMemory),
		set("beta", "VORNIK_URL", s.hermesURL), set("beta", "VORNIK_BROKER_TOKEN", bBroker), set("beta", "VORNIK_MEMORY_TOKEN", bMemory),
		set("beta", "API_SERVER_KEY", apiKeys["beta"]), set("beta", "OPENAI_API_KEY", "unused"),
		set("gamma", "API_SERVER_KEY", apiKeys["gamma"]), set("gamma", "OPENAI_API_KEY", "unused"),
	}, "\n")
	setup := exec.Command("podman", hermesRunArgs(t, "--rm", "--network", "host", "-v", home+":/opt/data:Z",
		hermesImage, "sh", "-c", script)...)
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("profile setup: %v\n%s", err, tail(string(out), 3000))
	}
	run(t, "podman", "unshare", "chown", "-R", "0:0", home)
	for _, p := range []string{"beta", "gamma"} {
		ph := filepath.Join(home, "profiles", p)
		writeFile(t, filepath.Join(ph, "config.yaml"), muxProfileConfig(modelName, baseURLs[p], false))
		copyPlugin(t, src, ph)
		env, err := os.ReadFile(filepath.Join(ph, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		// A setup check on gamma's own file, not the leak detector (the leak
		// is in the process environment; the audit rows catch it).
		if p == "gamma" && strings.Contains(string(env), "VORNIK_") {
			t.Fatalf("gamma's .env holds a Vornik setting before the gateway starts:\n%s", env)
		}
	}

	// One gateway for all three: the container's main process (--no-supervise).
	requireMemory(t, "before the Hermes gateway", hermesStageItems()...)
	gw := "vornik-e2e-hermes-gw-" + runSuffix
	startContainer(t, gw, memHermes, "--network", "host", "-v", home+":/opt/data:Z", hermesImage, "gateway", "run", "--no-supervise")
	base := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	waitFor(t, "the Hermes gateway's API server", slow(3*time.Minute), func() bool { return httpOK(base + "/health") })

	for _, p := range muxProfiles {
		models[p].Watch(muxTurnTag(p))
	}
	// The gateway's boot warm-up builds the LAUNCH profile's tool list in
	// the background (Hermes gateway/run_startup.py
	// _warm_turn_prerequisites), which runs the plugin's broker check with
	// default's key in default's own scope. /health answers before it ends,
	// and bring-up 2026-10-14 saw its catalog row land inside gamma's window.
	// It is default's own call, so wait for it to finish before S0.
	waitFor(t, "the Hermes gateway's boot warm-up", slow(2*time.Minute), func() bool {
		out, _ := exec.Command("podman", "exec", gw, "sh", "-c", "cat /opt/data/logs/*.log 2>/dev/null").Output()
		done, failed := gatewayWarmUpEnded(string(out))
		if failed {
			t.Log("the gateway's boot warm-up failed; the launch profile initializes on its first turn")
		}
		return done
	})
	snap := multiplexSnapshots{S0: muxAuditRows(t)}
	t.Logf("S0 settled at %s with %d rows", time.Now().UTC().Format("15:04:05.000"), len(snap.S0))
	muxTurn(t, base, "gamma", apiKeys["gamma"], modelName)
	snap.S1 = muxAuditRows(t)
	muxTurn(t, base, "beta", apiKeys["beta"], modelName)
	snap.S2 = muxAuditRows(t)
	muxTurn(t, base, "default", apiKeys["default"], modelName)
	snap.S3 = muxAuditRows(t)

	if peak, err := exec.Command("podman", "exec", gw, "cat", "/sys/fs/cgroup/memory.peak").Output(); err == nil {
		t.Logf("gateway container memory.peak: %s bytes (limit %d)", strings.TrimSpace(string(peak)), memHermes)
	}

	problems, denom := multiplexVerdict(keys, snap)
	t.Log(denom)
	for _, p := range muxProfiles {
		offered := vornikToolsOffered(models[p].ToolsSeen())
		t.Logf("%s was offered Vornik tools %v", p, offered)
		if !models[p].WatchSeen() {
			// Without this, gamma's "offered nothing" could mean its turn
			// never reached a model (or reached another profile's).
			problems = append(problems, fmt.Sprintf("%s's turn never reached %s's own model endpoint", p, p))
		}
		switch p {
		case "gamma":
			if len(offered) > 0 {
				problems = append(problems, fmt.Sprintf("gamma (no settings) was offered Vornik tools %v", offered))
			}
		default:
			for _, want := range []string{"vornik_catalog", "vornik_recall"} {
				if !strings.Contains(strings.Join(offered, ","), want) {
					problems = append(problems, fmt.Sprintf("%s was not offered %s (offered %v)", p, want, offered))
				}
			}
			// Not the stub's Failures(): Hermes also sends the model a
			// tool-less side request carrying the prompt (bring-up
			// 2026-10-14), which the script records as a step it could not
			// take. The audit rows and the offered tools are the evidence.
			if st := stubs[p]; st != nil {
				t.Logf("%s's scripted model: %d requests, unmatched steps %v", p, st.Requests(), st.Failures())
			}
		}
	}
	t.Logf("arm wall time %s", time.Since(start).Round(time.Second))
	if len(problems) > 0 {
		// The timeline, so a failure can be placed against the turns above.
		out, _ := exec.Command("podman", "exec", pgContainerName(), "psql", "-U", "vornik", "-d", "vornik", "-tAc",
			"SELECT to_char(created_at AT TIME ZONE 'UTC', 'HH24:MI:SS.MS'), task_id, project_id, tool_name FROM tool_audit_log "+
				"WHERE task_id LIKE 'companion:%' ORDER BY created_at").Output()
		t.Logf("companion audit timeline (UTC):\n%s", out)
		t.Fatalf("profiles did not keep their own Vornik settings:\n- %s", strings.Join(problems, "\n- "))
	}
}
